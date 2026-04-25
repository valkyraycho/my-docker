# my-docker — Learning Notes

Educational walkthrough of the project as it grows. Each section pairs the mental model with the code that implements it.

---

## Big picture: what is a "container" really?

A container isn't a VM. It's just a **normal Linux process** that has been given a carefully lied-to view of the system:

- Different PID numbering (`CLONE_NEWPID`)
- Different hostname (`CLONE_NEWUTS`)
- Different mounts / root filesystem (`CLONE_NEWNS` + `pivot_root`)
- Different IPC namespace (`CLONE_NEWIPC`)
- Resource caps enforced by the kernel (**cgroups v2**)
- A layered, copy-on-write filesystem (**OverlayFS**)

Docker orchestrates all of these. We're building that orchestration from scratch.

---

## The two-phase startup trick

One concept ties the whole codebase together: **a container boots in two phases**, because some setup (namespaces, cgroups) has to happen from the *outside* (parent), and some (pivot_root, mounting /proc) has to happen from the *inside* (child).

```
┌─────────────────────────┐        ┌─────────────────────────┐
│ Phase 1: PARENT (run)   │        │ Phase 2: CHILD (init)   │
│ ─────────────────────── │        │ ─────────────────────── │
│ overlay.Mount()         │        │ sethostname             │
│ cgroup.Create()         │  fork  │ pivot_root              │
│ exec /proc/self/exe ────┼───────►│ mount /proc /dev /sys   │
│   with CLONE_NEW*       │        │ mknod device nodes      │
│ cgroup.AddPID()         │        │ execve(user's command)  │
│ Wait()                  │        │                         │
└─────────────────────────┘        └─────────────────────────┘
```

The clever bit: `exec.Command("/proc/self/exe", "init", ...)` — the binary **re-invokes itself** with the `init` subcommand. Same binary, two very different jobs, chosen by `os.Args[1]` in `main.go`.

```go
// cmd/mydocker/main.go
switch os.Args[1] {
case "run":   runCommand(os.Args[2:])   // user-facing
case "init":  initCommand(os.Args[2:])  // internal, runs inside the new namespaces
}
```

> **Junior-dev gotcha:** why not do everything in the parent? Because namespaces are set when the child is *created* (via clone flags). You can only mount `/proc` correctly *after* you're in the new PID namespace, and you can only `pivot_root` *after* you're in the new mount namespace. So phase 2 exists by necessity, not by convenience.

---

## Milestone 1 — Process Isolation (namespaces)

**Mental model:** namespaces are the kernel's way of giving each process a private copy of some global resource (PID table, mount table, hostname, etc.).

```go
// internal/container/run.go
cmd := exec.Command("/proc/self/exe", append([]string{"init", rootfs}, args...)...)

cmd.SysProcAttr = &syscall.SysProcAttr{
    Cloneflags: syscall.CLONE_NEWPID | // new PID namespace → child sees itself as PID 1
                syscall.CLONE_NEWUTS | // new hostname namespace
                syscall.CLONE_NEWNS  | // new mount namespace → mounts don't leak to host
                syscall.CLONE_NEWIPC,  // new IPC namespace (shared memory, semaphores)
}
```

Then inside the child:

```go
// internal/container/init.go
unix.Sethostname([]byte("my-docker"))  // only affects the UTS namespace
```

**Trade-off noted:** we're *not* using `CLONE_NEWUSER` (user namespace) or `CLONE_NEWNET` (network). Docker uses both. `NEWUSER` is the hardest to get right; `NEWNET` is milestone 7.

---

## Milestone 2 — Filesystem Isolation (pivot_root)

**Mental model:** `chroot` is a suggestion; `pivot_root` is the real deal. It swaps what the process considers `/`, then we throw away the old root so the container can't escape by `cd ..`-ing out.

There are four landmines in this dance, and our code walks around each one:

```go
// internal/container/mount.go — setupRoot()

// 1. Mark our mount namespace PRIVATE so our mounts never propagate to the host
unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, "")

// 2. pivot_root requires new root to be a mount point → bind-mount rootfs onto itself
unix.Mount(rootfs, rootfs, "", unix.MS_BIND|unix.MS_REC, "")

// 3. The actual pivot: new root becomes /, old root ends up at /.old_root
unix.PivotRoot(rootfs, oldRoot)
os.Chdir("/")

// 4. Detach and delete the old root so the container can't see the host anymore
unix.Unmount("/.old_root", unix.MNT_DETACH)
os.RemoveAll("/.old_root")
```

Then populate the container's view of `/proc`, `/dev`, `/sys`:

```go
unix.Mount("proc",  "/proc", "proc",  0, "")  // /proc reflects the new PID namespace
unix.Mount("tmpfs", "/dev",  "tmpfs", 0, "")  // fresh in-memory /dev
unix.Mount("sysfs", "/sys",  "sysfs", 0, "")
```

And because a fresh tmpfs `/dev` is empty, we create the device nodes by hand:

```go
// {"/dev/null", S_IFCHR|0666, 1, 3}, {"/dev/zero", ..., 1, 5}, ...
unix.Mknod(n.path, n.mode, int(unix.Mkdev(n.major, n.minor)))
```

**Why `MS_PRIVATE` is non-negotiable:** systemd on modern distros makes mounts shared by default. Without the `MS_PRIVATE|MS_REC` line, every mount we do inside the container would propagate to the host. Painful discovery, usually.

---

## Milestone 3 — Resource Limits (cgroups v2)

**Mental model:** cgroups are directories in `/sys/fs/cgroup/`. We create a directory, write limits to files in it, write a PID to `cgroup.procs`, and the kernel enforces the caps. That's it — no daemon, no API, just a filesystem.

```
/sys/fs/cgroup/
├── cgroup.subtree_control     ← enable controllers for children
├── init/                       ← we stash root's processes here
└── mydocker/                   ← our parent group
    ├── cgroup.subtree_control
    └── <container-id>/
        ├── memory.max          ← "2097152" (2 MiB)
        ├── cpu.max             ← "50000 100000" (50% of one CPU)
        ├── pids.max            ← "100"
        └── cgroup.procs        ← write child PID here
```

```go
// internal/cgroup/cgroup.go
type Limits struct {
    MemoryBytes int64  // 0 = unlimited
    CPUPercent  int    // 0-100
    PidsMax     int
}

// Create: mkdir the container's cgroup, write limit files
os.MkdirAll(m.path, 0755)
writeFile(filepath.Join(m.path, "memory.max"), strconv.FormatInt(l.MemoryBytes, 10))
writeFile(filepath.Join(m.path, "cpu.max"),    formatCPU(l.CPUPercent))  // "50000 100000"
writeFile(filepath.Join(m.path, "pids.max"),   strconv.Itoa(l.PidsMax))

// AddPID: this is the moment limits start applying
func (m *Manager) AddPID(pid int) error {
    return writeFile(filepath.Join(m.path, "cgroup.procs"), strconv.Itoa(pid))
}
```

**The "no internal processes" rule — and why `prepareRoot()` exists:**

cgroups v2 enforces that a cgroup with child cgroups cannot itself have processes. But on a fresh system, every PID lives in the root cgroup. So before we can enable controllers on the root, we have to **evacuate** root's processes into a sibling `init` cgroup:

```go
// prepareRoot()
initCgroup := filepath.Join(root, "init")
os.MkdirAll(initCgroup, 0755)

procs, _ := os.ReadFile(filepath.Join(root, "cgroup.procs"))
for pid := range strings.FieldsSeq(string(procs)) {
    _ = os.WriteFile(initProcs, []byte(pid), 0644)  // ignore EBUSY for kthreads
}

writeFile(filepath.Join(root, "cgroup.subtree_control"), "+memory +cpu +pids")
```

**Ordering matters in `Run`:**

```go
cg.Create(limits)       // 1. cgroup exists with limits set
cmd.Start()             // 2. child is born (still unconstrained for a blink)
cg.AddPID(cmd.Pid)      // 3. child is caged
cmd.Wait()
```

> **Trade-off:** there's a tiny window between step 2 and 3 where the child isn't in the cgroup yet. A malicious workload could fork-bomb in that window. Docker uses `clone3(CLONE_INTO_CGROUP)` to avoid it. Good enough for a learning clone.

---

## Milestone 4 — Layered Filesystem (OverlayFS)

**Mental model:** OverlayFS is how Docker images are fast and small. Multiple read-only "lower" layers are stacked; a writable "upper" sits on top; the kernel presents a unified "merged" view. Writes go to upper (copy-on-write).

```
     merged/        ← what the container sees (/ inside the container)
       ▲
       │ union
       │
   ┌───┴───┐
   │ upper │       ← writable, container-specific changes
   └───┬───┘
       │
   ┌───┴───┐
   │ lower │       ← read-only, shared between containers (the "image")
   └───────┘
   + work/         ← overlayfs scratch dir, kernel-owned
```

```go
// internal/overlay/overlay.go
// Reverse layer order: overlayfs wants top-most layer FIRST in lowerdir
paths := make([]string, len(layerNames))
for i, name := range layerNames {
    paths[len(layerNames)-1-i] = filepath.Join(layersDir, name)
}
lowerdir := strings.Join(paths, ":")

options := fmt.Sprintf(
    "lowerdir=%s,upperdir=%s,workdir=%s",
    lowerdir, upperPath, workPath,
)
unix.Mount("overlay", mergedPath, "overlay", 0, options)
```

**Why `EnsureRoot` mounts `containers/` as tmpfs — but *not* the rest:**

```go
// root and layersDir are regular, on-disk directories (persistence needed)
for _, d := range []string{root, layersDir} {
    os.MkdirAll(d, 0755)
}

// Only containersDir is backed by tmpfs — upper/work/merged are throwaway
os.MkdirAll(containersDir, 0755)
if !mounted {
    unix.Mount("tmpfs", containersDir, "tmpfs", 0, "")
}
```

This is a **deliberately narrowed scope**. The v1 of this code mounted tmpfs on all of `/var/lib/mydocker`, which worked fine *until* milestone 5 started pulling images. Then the bug showed up:

```
1. mydocker pull alpine:3.19 → writes into /var/lib/mydocker/layers/
2. mydocker run <layer> ...  → calls EnsureRoot()
   → mounts tmpfs on /var/lib/mydocker
   → tmpfs is empty → shadows the pulled layers
   → Mount() reports "layer not found"
```

The pulled layers weren't deleted — they were just *hidden* by the fresh tmpfs overlaying the directory. Unmount the tmpfs and they'd reappear. Lesson: **a mount that shadows real state is a silent data-loss bug waiting to happen.**

The fix is to put each subdirectory on the filesystem that matches its lifecycle:

| Directory | Backing | Lifecycle |
|---|---|---|
| `/var/lib/mydocker/` | disk | persistent (configured once) |
| `layers/` | disk | persistent (image cache — pulled once, reused) |
| `blobs/`, `images/` | disk | persistent (managed by the image package) |
| `containers/` | **tmpfs** | ephemeral (one entry per container, cleaned on reboot) |

Why tmpfs for `containers/` specifically? Two reasons:

1. **OverlayFS needs its upper/work dirs on a "real" filesystem** — some host filesystems (like the one Docker-in-Docker gives you) aren't overlayfs-compatible. Mounting tmpfs there sidesteps stacking restrictions.
2. **Container writable layers are meant to die with the container.** Backing them with tmpfs means a reboot automatically cleans up anything `Unmount` didn't.

**The shutdown path is a closure over the ID:**

```go
// cmd/mydocker/main.go
mergedPath, err := overlay.Mount(containerID, []string{layer})
defer func() {
    if err := overlay.Unmount(containerID); err != nil {
        fmt.Fprintf(os.Stderr, "cleanup: %v\n", err)
    }
}()
```

> **Docker parity note:** real Docker layers come from OCI image tarballs (milestone 5, now implemented!). `layers/` on disk is exactly how Docker does it.

---

## Milestone 5 — OCI Image Pulling

**Mental model:** a container registry (Docker Hub, GHCR, etc.) is just an HTTP server that speaks the **OCI Distribution Spec**. Two resource types matter:

- **Manifests** — small JSON documents describing an image (config digest + ordered list of layer digests + platform).
- **Blobs** — the actual bytes, addressed by their SHA-256 digest (`sha256:abc…`). Immutable and content-addressable — the digest *is* the integrity check.

Pulling an image = "fetch a manifest, then fetch each blob it references, verify digests, extract tarballs." That's the whole milestone.

```
GET /v2/library/alpine/manifests/3.19        →  Index (multi-arch list) OR Manifest
GET /v2/library/alpine/manifests/<digest>    →  Manifest for one platform
GET /v2/library/alpine/blobs/sha256:<cfg>    →  config blob (image metadata JSON)
GET /v2/library/alpine/blobs/sha256:<lyr1>   →  layer 1 (gzipped tarball)
GET /v2/library/alpine/blobs/sha256:<lyr2>   →  layer 2 (gzipped tarball)
...
```

### The two new packages

```
internal/registry/           ← "talk to an OCI registry over HTTP"
├── manifest.go              OCI types + media-type constants
├── auth.go                  bearer-token challenge/response
└── client.go                GetManifest, GetBlob, doAuthed (401 retry)

internal/image/              ← "store and manipulate image data locally"
├── store.go                 on-disk layout + path computation
├── fetch.go                 FetchBlob (streaming SHA256) + ExtractLayer (untar)
└── pull.go                  Pull orchestrator: ref parsing → platform → layers
```

This split matters. `registry/` is pure HTTP — it has no idea where bytes end up. `image/` owns the disk. If we later add a local image import (skip the registry, import a tarball), only `image/` participates.

---

### 5a · Registry HTTP client

#### Manifests vs Indexes — the multi-arch wrinkle

Real registries serve two different things at the same tag:

- A **Manifest** describes *one* image (one platform: `linux/amd64`, say).
- An **Index** (a.k.a. "manifest list") is a list of manifests, one per platform.

When you GET `alpine:3.19`, you usually get an *index* first; you then pick the manifest whose `Platform` matches yours, and GET *that* manifest.

```go
// internal/registry/manifest.go
type Descriptor struct {
    MediaType string
    Digest    string    // "sha256:..."
    Size      int64
    Platform  *Platform // only on index entries
}
type Manifest struct { Config Descriptor; Layers []Descriptor }
type Index    struct { Manifests []Descriptor }
```

The four media-type constants exist because registries serve both OCI and legacy Docker variants:

```go
MediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
MediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
MediaTypeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
MediaTypeDockerIndex    = "application/vnd.docker.distribution.manifest.list.v2+json"
```

Advertise all four in `Accept`, let the registry pick, and **trust the HTTP `Content-Type` header** to classify the response — not the `mediaType` field inside the JSON (some registries lie in the body):

```go
// internal/registry/client.go — GetManifest
req.Header.Set("Accept", strings.Join([]string{ /* all four */ }, ", "))
// ...
mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
return mediaType, body, nil
```

#### Bearer-token auth — the "try, 401, token, retry" dance

Even public images on Docker Hub require a bearer token. The flow:

```
1. GET /v2/library/alpine/manifests/3.19      (no auth)
2. ← 401 Unauthorized
   WWW-Authenticate: Bearer realm="...auth.docker.io/token",
                            service="registry.docker.io",
                            scope="repository:library/alpine:pull"
3. GET <realm>?service=...&scope=...           (exchange for token)
4. ← 200 {"token": "eyJ..."}
5. GET /v2/library/alpine/manifests/3.19       Authorization: Bearer eyJ...
6. ← 200 {...manifest...}
```

Encoded as a *transparent retry* in `doAuthed`, with token caching so subsequent calls skip the 401 round-trip:

```go
func (c *Client) doAuthed(req *http.Request) (*http.Response, error) {
    if c.token != "" {
        req.Header.Set("Authorization", "Bearer "+c.token)
    }
    resp, _ := c.http.Do(req)
    if resp.StatusCode != http.StatusUnauthorized {
        return resp, nil   // happy path — no retry
    }

    ch, _   := parseChallenge(resp.Header.Get("WWW-Authenticate"))
    token, _ := fetchToken(c.http, ch)
    c.token = token                     // cache for next call

    req = req.Clone(req.Context())      // clone: Do() has consumed the original
    req.Header.Set("Authorization", "Bearer "+c.token)
    return c.http.Do(req)
}
```

**Design points worth naming:**

- **Token cached on `*Client`** — subsequent calls skip the 401 round-trip. Trade-off: *not* safe for concurrent use without a mutex. Good enough for now.
- **`GetBlob` returns `io.ReadCloser`, not `[]byte`.** Layers are hundreds of MB. Streaming lets `fetch.go` pipe bytes straight through `sha256.Hash → file → gzip → tar`, never loading the full blob into memory. Good habit to form early — memory-bounded IO is a real engineering win.
- **Parsing the challenge with a regex on `key="value"`** (`challengeParamRE`) works, but a spec-compliant parser would handle escaped quotes and commas. Fine for Docker Hub, flimsy for the real world. Noted as a limitation.
- **Token response falls back to `access_token`** if `token` is empty — the spec allows both, so we check both.

---

### 5b · Image store — the three-level layout

```
/var/lib/mydocker/
├── blobs/          ← raw downloaded bytes (compressed tarballs + config JSON)
│   └── sha256_abc123.../
│       └── data
│
├── layers/         ← extracted layer trees, ready to overlay-mount
│   └── sha256_abc123.../
│       └── bin/ etc/ usr/ ...
│
└── images/         ← human-name → digests mapping
    └── library_alpine_3.19/
        ├── manifest.json
        └── config.json
```

Three categories, each doing one job:

| Directory | Keyed by | Purpose |
|---|---|---|
| `blobs/` | digest | Content-addressable raw bytes. Source of truth — the download cache. |
| `layers/` | digest | Extracted tree, ready for `overlay.Mount`. Derivable from the blob. |
| `images/` | name:tag | Human-pointer → list of digests (so we can answer "what layers make up `alpine:3.19`?"). |

**Why separate `blobs/` from `layers/`?**

- `blobs/` is the immutable download cache. Pull twice → skip the second download.
- `layers/` is the runtime-usable form. In principle rebuildable from the blob.
- Keeping them split means independent GC, and it matches real Docker's internal split between `/var/lib/docker/image/.../layerdb/` and `/var/lib/docker/overlay2/`.

#### The filename-escaping problem

Digests are `sha256:abc…`. The **colon is a path separator on some filesystems** and always looks weird in paths. So:

```go
func digestPath(digest string) string {
    return strings.ReplaceAll(digest, ":", "_")  // sha256:abc → sha256_abc
}
```

Similarly, image names contain `/` (`library/alpine`), so we flatten those too.

---

### 5c · Streaming fetch + SHA256 verification — `fetch.go`

This is the cleanest pattern in the milestone. **Verify the digest *while* you download** — don't re-read the whole file afterward.

```go
// internal/image/fetch.go
body, _ := client.GetBlob(repo, expectedDigest)  // io.ReadCloser
defer body.Close()

f, _ := os.Create(tmpBlobPath)
hasher := sha256.New()

// The key line: TeeReader splits the stream — one copy to the file,
// one copy to the hasher. No intermediate buffering.
io.Copy(f, io.TeeReader(body, hasher))

hexGot := hex.EncodeToString(hasher.Sum(nil))
if hexGot != hexWant {
    os.Remove(tmpBlobPath)          // quarantine corrupt downloads
    return fmt.Errorf("digest mismatch")
}
os.Rename(tmpBlobPath, blobPath)     // atomic commit
```

Three things to notice:

1. **`io.TeeReader`** is the Go idiom for "do two things with one stream." Memory use stays O(buffer_size), not O(blob_size).
2. **Temp file + rename** gives you atomicity. A crash mid-download leaves `.tmp` garbage, not a half-written blob masquerading as valid. `os.Rename` on the same filesystem is atomic.
3. **Verify before rename.** If the digest doesn't match, the file never becomes a blob. Future pulls will re-download instead of trusting corrupt data.

`★ Insight ─────────────────────────────────────`
This "stream-hash-commit" pattern shows up everywhere: downloads, uploads, cache fills, log writers. Once you internalize it, you'll notice code that *doesn't* do this — code that buffers a whole response into memory, or writes directly to the final path — is almost always buggy under load or crashes.
`─────────────────────────────────────────────────`

#### Extracting the layer tarball

`ExtractLayer` is the same pattern, shifted up a level: open blob → `gzip.NewReader` → `tar.NewReader` → walk entries.

```go
// internal/image/fetch.go — ExtractLayer, simplified
f, _  := os.Open(blobPath)
gz, _ := gzip.NewReader(f)
tr    := tar.NewReader(gz)

for {
    hdr, err := tr.Next()
    if err == io.EOF { break }

    target := filepath.Join(tmpDestLayerPath, hdr.Name)

    // Security: reject entries that escape the destination
    if !strings.HasPrefix(target, filepath.Clean(tmpDestLayerPath)+string(os.PathSeparator)) {
        return fmt.Errorf("tar entry escapes dest: %s", hdr.Name)
    }

    // Skip whiteouts (overlayfs deletion markers)
    if strings.HasPrefix(filepath.Base(hdr.Name), ".wh.") { continue }

    switch hdr.Typeflag {
    case tar.TypeDir:     os.MkdirAll(target, mode.Perm())
    case tar.TypeReg:     io.Copy(createFile(target, mode), tr)
    case tar.TypeSymlink: os.Symlink(hdr.Linkname, target)
    case tar.TypeLink:    os.Link(filepath.Join(tmpDestLayerPath, hdr.Linkname), target)
    }
}
os.Rename(tmpDestLayerPath, destLayerPath)  // again: atomic commit
```

Four things earning their keep here:

1. **The escape check** (`strings.HasPrefix(target, ...)`) is defense against **"tar slip"** / zip-slip attacks — a malicious tarball containing `../../../etc/passwd` that would overwrite host files. Never trust the archive; always re-validate the resolved path.
2. **Whiteout skip (`.wh.`)** — overlayfs uses `.wh.foo` files inside a tarball to mean "delete `foo` from lower layers." We're extracting layers independently, not stacking them semantically, so we just skip them. When we stack with `overlay.Mount`, overlayfs itself handles deletions.
3. **Named return for cleanup** (`retErr error` + deferred `RemoveAll`) — if any step fails, the `.tmp` directory is removed. Classic Go pattern for resource cleanup with errors.
4. **Atomic rename at the end** — same story as `FetchBlob`. A half-extracted layer never enters `layers/`.

> **Junior-dev gotcha noted:** our extraction doesn't set uid/gid from the tar header. That's fine because the container will run under the host's root anyway (no user namespaces yet), but it'd matter if we add `CLONE_NEWUSER` later.

---

### 5d · Pull orchestration — `pull.go`

Ties everything together:

```go
func (s *Store) Pull(client *registry.Client, ref string) error {
    repo, tag := parseRef(ref)                       // "alpine:3.19" → ("library/alpine", "3.19")
    s.EnsureDirs()

    mediaType, manifestBytes, _ := client.GetManifest(repo, tag)

    // If it's an index, resolve to our platform's manifest
    if isIndex(mediaType) {
        var index registry.Index
        json.Unmarshal(manifestBytes, &index)
        entry := matchPlatform(&index)               // linux/GOARCH match
        if entry == nil { return fmt.Errorf("no manifest for %s/%s", runtime.GOOS, runtime.GOARCH) }
        mediaType, manifestBytes, _ = client.GetManifest(repo, entry.Digest)
    }

    var manifest registry.Manifest
    json.Unmarshal(manifestBytes, &manifest)

    // Fetch config blob (no extraction — it's JSON, not a tarball)
    s.FetchBlob(client, repo, manifest.Config.Digest)
    configBytes, _ := os.ReadFile(s.BlobPath(manifest.Config.Digest))

    // Fetch + extract each layer
    for _, layer := range manifest.Layers {
        s.FetchBlob(client, repo, layer.Digest)
        s.ExtractLayer(layer.Digest)
    }

    // Save the human-name → bytes mapping
    s.SaveImage(repo, tag, manifestBytes, configBytes)
    return nil
}
```

**Reference parsing** is worth calling out — Docker's shorthand conventions are baked in:

```go
func parseRef(ref string) (string, string) {
    repo, tag, ok := strings.Cut(ref, ":")
    if !ok { tag = "latest" }                       // "alpine" → tag "latest"
    if !strings.Contains(repo, "/") {
        repo = "library/" + repo                    // "alpine" → "library/alpine"
    }
    return repo, tag
}
```

`alpine` → `library/alpine:latest`. `alpine:3.19` → `library/alpine:3.19`. Matches Docker CLI behavior, so users get what they expect.

**Platform matching** uses `runtime.GOOS` + `runtime.GOARCH` — so `GOARCH=arm64 go build` pulls arm64 images, `GOARCH=amd64` pulls amd64. Good alignment with Go's build model.

---

### Three bugs caught during M5 — worth remembering

1. **Path concat without separator** (`root + "blobs"` = `/var/lib/mydockerblobs`, not `/var/lib/mydocker/blobs`). Silent creation of sibling directories; worse, `image/` and `overlay/` disagreed on where `layers/` lived. **Moral: use `filepath.Join` or always include the leading `/`.**
2. **Inverted predicate logic** (`HasBlob` returned `err != nil`, meaning "true when file is missing"). **Moral: `Has*/Is*/Can*` functions deserve a 3-line sanity test.**
3. **Tmpfs shadowing pulled layers** — the M4 update discussed above. **Moral: a mount that shadows persistent data is a silent data-loss bug.**

Two general-form lessons emerge:
- **Every cross-package shared path constant is a liability.** If `image.layersDir` and `overlay.layersDir` drift, you get runtime "not found" errors with no compile-time warning. A single source of truth (a shared config package) would prevent this class of bug.
- **Predicates and resource-lifecycle code benefit disproportionately from trivial tests.** They're where the most silent bugs hide.

---

## Milestone 6 — Container Lifecycle (`run -d`, `ps`, `logs`, `stop`, `rm`)

**The shift in mental model:** up through M5, a container was a *function call* — `Run()` blocks until the workload exits, then tears everything down. M6 makes a container a **managed resource with a lifecycle**: it's created, runs (maybe in the background), eventually exits, and stays listable/inspectable until someone removes it.

This requires three things we didn't have before:

1. **Persistent state** — a record of every container that ever ran, not just the one in front of us right now.
2. **Identity that survives the process** — so a later `mydocker stop abc` can find the right PID and send it a signal safely.
3. **A "PID 1"** — a real init process inside the container that owns signal forwarding and (in principle) zombie reaping.

The rest of M6 is how each piece falls out of those three requirements.

---

### 6a · The state package — who's alive, what ran, how it ended

```
internal/state/
├── state.go     ← Container struct + Save/Load
├── proc.go      ← ReadStartTime, IsRunning (the PID-reuse defense)
└── registry.go  ← List (scan dir), Find (prefix match)
```

#### The `Container` struct — what we persist

```go
// internal/state/state.go
type Container struct {
    // Identity
    ID      string   // 12-char hex generated at run-time
    Image   string   // "library/alpine:3.19" — for display
    Layers  []string // digest paths, already in top-first order
    Command []string // argv for `ps` display

    // Runtime identity — the (PID, StartTime) tuple
    PID       int
    StartTime uint64 // /proc/<pid>/stat field 22 (jiffies since boot)

    // Lifecycle
    Status     string    // "running" | "exited"
    ExitCode   int
    CreatedAt, StartedAt, FinishedAt time.Time
}
```

Saved as pretty-printed JSON at `/var/lib/mydocker/containers/<id>/state.json`. One file per container. No database, no index — just a directory of JSON files.

#### The `(PID, StartTime)` tuple — defending against PID reuse

This is the most important idea in M6. **PIDs get reused.** After process 12345 exits, the kernel will eventually assign 12345 to someone else — maybe your browser, maybe a daemon. If we stored only the PID and later did `kill(12345, SIGTERM)`, we'd SIGTERM *whatever* has PID 12345 right now, not our container. Catastrophic.

The fix: record a **second coordinate** that's unique-per-process and doesn't collide under reuse — the process's **start time**, measured in jiffies since boot. Linux exposes this as field 22 of `/proc/<pid>/stat`:

```go
// internal/state/proc.go
func ReadStartTime(pid int) (uint64, error) {
    b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
    s := string(b)

    // /proc/<pid>/stat looks like:
    //   1234 (comm name with spaces) R 1 1234 1234 ...
    //                                ^ we split from here
    // Field 2 — the `(comm)` — can contain spaces AND parentheses.
    // Safe split: find the LAST ')'.
    lastParamIdx := strings.LastIndexByte(s, ')')
    tail := s[lastParamIdx+2:]
    fields := strings.Fields(tail)
    // After the ')', field 22 becomes index 19 (we removed the first two).
    return strconv.ParseUint(fields[19], 10, 64)
}

func IsRunning(pid int, wantStart uint64) bool {
    gotStart, err := ReadStartTime(pid)
    if err != nil { return false }       // stat missing → process gone
    return gotStart == wantStart          // same PID + same start time → really us
}
```

`★ Insight ─────────────────────────────────────`
- **Why "last `)`", not "second `)`":** the `comm` field is whatever the process named itself — a user could literally `prctl(PR_SET_NAME, "evil) 1 2 3 4 ...")` and inject fake fields. This is the hardening note every kernel parser has. `LastIndexByte(s, ')')` guarantees we're past the adversary-controlled section.
- **Why jiffies and not wall-clock time:** jiffies since boot are monotonic — they can't go backwards, survive clock adjustments, and are the kernel's own internal measure. Wall clock (`CreatedAt`) is for humans; jiffies are for correctness.
- **This `(pid, start_time)` pattern is the standard process-identity trick** used by systemd (`PIDFDFSelfRef`), Kubernetes' kubelet, and tini. Internalize it — you'll recognize it everywhere once you know to look.
`─────────────────────────────────────────────────`

#### Atomic save

The same temp-file-then-rename trick you saw in M5:

```go
// internal/state/state.go — Save
b, _ := json.MarshalIndent(c, "", "  ")
os.WriteFile(tmpStatePath, b, 0644)
os.Rename(tmpStatePath, statePath)    // atomic on same filesystem
```

A crash mid-write leaves a `.tmp` file, not a half-written `state.json`. A reader never sees a partially-written file.

#### Prefix-match ID lookup — the UX polish

Docker lets you type `docker stop a3f` instead of the full 12+ char ID. `state.Find` replicates this:

```go
// internal/state/registry.go — Find
matches := []*Container{...everyone whose ID starts with prefix...}
switch len(matches) {
case 0:  return nil, fmt.Errorf("no such container: %s", prefix)
case 1:  return matches[0], nil
default: return nil, fmt.Errorf("ambiguous prefix %q matches %d containers: %s", ...)
}
```

The three-way return is a good instinct: unambiguously succeed, cleanly fail, **refuse ambiguity** rather than guessing. Silently picking the "first" match is the kind of fallback that bites you at 3am.

---

### 6b · `image.Resolve` — running from a local image, not a layer digest

Before M6, `run` took a layer *directory name* as its positional. Now it takes an image ref (`alpine:3.19`) and we resolve it.

```go
// internal/image/resolve.go
func (s *Store) Resolve(ref string) ([]string, error) {
    repo, tag := parseRef(ref)

    manifestBytes, err := os.ReadFile(filepath.Join(s.ImageDir(repo, tag), "manifest.json"))
    if errors.Is(err, os.ErrNotExist) {
        return nil, fmt.Errorf("image %q not found: %w", ref, ErrImageNotFound)
    }

    var manifest registry.Manifest
    json.Unmarshal(manifestBytes, &manifest)

    // Reverse: manifest lists base→top; overlayfs wants top→base in lowerdir.
    result := make([]string, len(manifest.Layers))
    for i, layer := range manifest.Layers {
        result[len(manifest.Layers)-i-1] = digestPath(layer.Digest)
    }
    return result, nil
}
```

Two things worth naming:

1. **The reversed layer order** answers one of the open questions from M4: overlayfs's `lowerdir=a:b:c` resolves files top-down (first listed wins), but OCI manifests list layers *bottom-up* (base image first, app layer last). So we reverse. Getting this wrong silently masks files with older versions of themselves.
2. **`ErrImageNotFound` as a sentinel error** — `run` catches this specifically and auto-pulls:

    ```go
    // cmd/mydocker/main.go — runCommand
    layers, err := store.Resolve(ref)
    if errors.Is(err, image.ErrImageNotFound) {
        fmt.Fprintf(os.Stderr, "image %q not found locally, pulling...\n", ref)
        store.Pull(client, ref)
        layers, err = store.Resolve(ref)
    }
    ```

   Matches `docker run`'s behavior of transparently pulling missing images. Using a sentinel (rather than string-matching the error message) is the right call — it survives error-message rewording.

---

### 6c · Detach mode (`run -d`) — the five differences from foreground

Most of the `-d` logic lives in `run.go`. Reading the diff between foreground and detached paths is a great exercise in understanding what "running in the background" actually *means*:

```go
// internal/container/run.go
if opts.Detach {
    // Difference 1: stdout/stderr → log files (not terminal)
    stdoutLog, _ := os.OpenFile(state.StdoutPath(opts.ContainerID), ...)
    stderrLog, _ := os.OpenFile(state.StderrPath(opts.ContainerID), ...)
    cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, stdoutLog, stderrLog

    // Difference 2: Setsid — start a new session, detach from parent's controlling terminal
    cmd.SysProcAttr.Setsid = true
} else {
    cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
}

// ... Start, cgroup add, state save ...

if opts.Detach {
    return nil     // Difference 3: don't Wait — exit and leave the child running
}
// foreground path: Wait, update state on exit, defer cleanup
```

And over in `cmd/mydocker/main.go`:

```go
if !*detach {
    defer overlay.Unmount(containerID)    // Difference 4: skip unmount in detach
}
// ... Run ...
if *detach {
    fmt.Println(containerID)              // Difference 5: print the ID, like `docker run -d`
}
```

**Why `Setsid: true` is non-negotiable for detach:** without it, the container process stays in the parent shell's *session*. When the user closes the terminal, the kernel sends `SIGHUP` to the session leader, which propagates to every process in the session — including your "detached" container. It dies the moment you close the terminal.

`Setsid` creates a new session with the child as session leader, severing that link. This is what `nohup`, `setsid(1)`, and every well-behaved daemon does.

**Why we skip `overlay.Unmount` in detach:** in foreground mode, `defer overlay.Unmount(id)` fires when the user's process exits. In detach, the parent returns *immediately* after `cmd.Start()` — the child is still running. Unmounting now would pull the filesystem out from under a live container. Cleanup is deferred until `rm`.

**Also notice what `Unmount` no longer does:**

```go
// internal/overlay/overlay.go — Unmount (post-M6)
func Unmount(containerID string) error {
    // ... unmount the overlay ...
    // NOTE: used to also RemoveAll(containerDir). Now only state.RemoveDir does that,
    // because containerDir holds state.json + stdout.log + stderr.log — we can't
    // delete them when a foreground container exits; they must survive until `rm`.
    return nil
}
```

A small refactor with big consequences: responsibility for removing the state directory moved to `rm.go`. Foreground containers now leave their state behind after exit — which is exactly what lets you `mydocker ps -a` to see exited containers, and `mydocker logs <id>` after the fact (for detached ones).

---

### 6d · `init.go` — becoming a real PID 1

Before M6:

```go
// old: the init binary REPLACES itself with the user's command
return unix.Exec(binary, args, os.Environ())
```

After M6:

```go
// new: init STAYS ALIVE as PID 1 and spawns the user command as a child
cmd := exec.Command(binary, args[1:]...)
cmd.Start()

sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, SIGTERM, SIGINT, SIGQUIT, SIGHUP, SIGUSR1, SIGUSR2)
go func() {
    for sig := range sigCh { _ = cmd.Process.Signal(sig) }
}()

err = cmd.Wait()
os.Exit(cmd.ProcessState.ExitCode())
```

**Why the change?** In a container with `CLONE_NEWPID`, the first process *is* PID 1 — the kernel's init process from that namespace's perspective. PID 1 has special responsibilities:

1. **Signal default handlers don't apply.** The kernel refuses to deliver signals whose only handler is the default to PID 1. If init doesn't explicitly register a handler for `SIGTERM`, `kill 1` does *nothing*. That's why `docker stop` on a naive image often seems to hang for 10 seconds then SIGKILL.
2. **Orphan reaping.** When a process's parent dies, its children re-parent to PID 1, which must `wait()` for them when they exit. Without reaping, zombies accumulate until you run out of PIDs.

Our new `Init` handles #1 (signal forwarding for a long list of signals) but only partially handles #2 — `cmd.Wait()` reaps the *direct* child only. If the user's workload forks grandchildren that later get orphaned, they re-parent to our init and become zombies that nothing reaps. A fully-correct init would loop `syscall.Wait4(-1, ..., WNOHANG, nil)` on every `SIGCHLD`. This is exactly what `tini` and `dumb-init` exist to do.

**Why that's OK for M6:** our containers are mostly `sh` or short-lived commands. The common case is no grandchildren, and if there are, they die when `sh` does. The limitation is real but minor — noted as an open question.

> **Junior-dev gotcha to internalize:** "PID 1 is special" is not a rule you'll find from reading userspace code — it's kernel behavior. When you inherit a container that "mysteriously doesn't respond to SIGTERM," the answer is almost always: no signal handler on PID 1.

---

### 6e · `ps` — list + lazy reconciliation

```go
// internal/container/ps.go
func Ps(w io.Writer, showAll bool) error {
    containers, _ := state.List()

    // Reconciliation: state says "running" but the process is actually gone
    for _, c := range containers {
        if c.Status == state.StatusRunning && !state.IsRunning(c.PID, c.StartTime) {
            c.Status = state.StatusExited
            c.FinishedAt = time.Now()
            c.Save()
        }
    }

    // tabwriter gives aligned columns without manual padding
    tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
    fmt.Fprintln(tw, "CONTAINER ID\tIMAGE\tCOMMAND\tSTATUS\tCREATED")
    for _, c := range containers {
        if !showAll && c.Status != state.StatusRunning { continue }
        fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", shortID(c.ID), ...)
    }
    return tw.Flush()
}
```

**The reconciliation insight:** state is *eventually consistent*, not live. A foreground container exited cleanly → `Run()` updated its state to "exited". But:

- If `mydocker` itself was killed (e.g., Ctrl-\), the child kept running and nobody updated state.
- In detach mode, the child can exit at any time; we're not watching.
- `FinishedAt = time.Now()` here is approximate — we're stamping the time *we noticed*, not when it actually died. Good enough for humans; wrong for forensics.

So `ps` heals the state on every invocation. Any command that lists containers is a good place to do this — it's cheap (one `open()` per PID) and amortizes the cost across natural user actions.

`★ Insight ─────────────────────────────────────`
**"Lazy reconciliation at read time"** is how a huge class of distributed systems avoid background reaper threads. Kubernetes does it, etcd does it, your container runtime does it. The principle: "don't spend CPU tracking state changes you don't need to react to immediately; detect them when you care." For a single-binary CLI tool, no reaper daemon is absolutely the right call.
`─────────────────────────────────────────────────`

---

### 6f · `logs` — just copy the file

```go
// internal/container/logs.go
func Logs(w io.Writer, prefix string) error {
    c, _ := state.Find(prefix)

    f, err := os.Open(state.StdoutPath(c.ID))
    if err != nil {
        return fmt.Errorf("no logs for %s — foreground container?", c.ID)
    }
    defer f.Close()

    _, err = io.Copy(w, f)
    return err
}
```

Almost trivially short, because **the hard work was done in `run.go`**: redirecting stdout/stderr to `stdout.log`/`stderr.log` at start time means `logs` is just `cat`.

The error message when the log file is missing is deliberately diagnostic, not just `"file not found"`. Why? Because there's exactly one case where this fails: the container ran in foreground and therefore wrote to the terminal, not to a file. Telling the user *why* saves them a round trip of "why doesn't my log file exist?". **Always write error messages that answer the next question.**

Limitation noted: `io.Copy` reads to EOF and returns. We don't support `docker logs -f` (follow/tail). Adding that would mean `inotify` or a polling loop — a natural M6.5 exercise.

---

### 6g · `stop` — the TERM-wait-KILL dance

```go
// internal/container/stop.go
const DefaultStopTimeout = 10 * time.Second

func Stop(prefix string, timeout time.Duration) error {
    c, _ := state.Find(prefix)

    if !state.IsRunning(c.PID, c.StartTime) {
        return reconcileExited(c)     // already dead; just fix up state
    }

    // 1. Polite: ask to stop
    if err := unix.Kill(c.PID, unix.SIGTERM); err != nil {
        if !errors.Is(err, unix.ESRCH) { return err }   // ESRCH = already gone, not an error
    }

    // 2. Wait up to `timeout` for it to exit gracefully
    if waitForExit(c, timeout) {
        return reconcileExited(c)
    }

    // 3. Impolite: force it
    if err := unix.Kill(c.PID, unix.SIGKILL); err != nil {
        if !errors.Is(err, unix.ESRCH) { return err }
    }

    // 4. Short final wait, then reconcile regardless
    waitForExit(c, time.Second)
    return reconcileExited(c)
}
```

This is the exact algorithm `docker stop` uses, and the algorithm `kubectl delete` uses (as "graceful termination"). Every container runtime ships some form of it.

**Why `ESRCH` is ignored:** between `IsRunning` returning true and `Kill` running, the process could have exited on its own. `kill()` returns `ESRCH` ("no such process") — that's a *success* from our perspective (the container is dead, which is what we wanted). Conflating it with a real error would make stop randomly fail when the container was already stopping.

**The polling inside `waitForExit`:**

```go
ticker := time.NewTicker(100 * time.Millisecond)
for {
    select {
    case <-ctx.Done():         return false
    case <-ticker.C:
        if !state.IsRunning(c.PID, c.StartTime) { return true }
    }
}
```

100ms is a sweet spot: fast enough to feel instant to humans, slow enough that we're not burning CPU in a `/proc` read loop. A fancier implementation would use `pidfd_open` + `poll` to get an event-driven wake (Linux 5.3+), but polling is 5 lines and "good enough" is a feature.

**Why `(PID, StartTime)` matters in `stop` specifically:** imagine the container exited 10 seconds ago, the kernel reused its PID for another process, and now you type `mydocker stop abc123`. Without the StartTime check, `waitForExit` would see *some* process with that PID and `IsRunning` would say yes forever. The StartTime check causes it to say "no, that's not our process" immediately. Same mechanism defends the SIGTERM itself from hitting a stranger.

---

### 6h · `rm` — cleanup with error aggregation

```go
// internal/container/rm.go
func Rm(prefix string, force bool) error {
    c, _ := state.Find(prefix)

    if state.IsRunning(c.PID, c.StartTime) {
        if !force {
            return errors.New("container is running; stop first or use -f")
        }
        Stop(prefix, DefaultStopTimeout)
    }

    var errs []error
    if err := overlay.Unmount(c.ID); err != nil {
        errs = append(errs, fmt.Errorf("unmount overlay: %w", err))
    }
    if err := cgroup.New(c.ID).Destroy(); err != nil {
        errs = append(errs, fmt.Errorf("destroy cgroup: %w", err))
    }
    if err := state.RemoveDir(c.ID); err != nil {
        errs = append(errs, fmt.Errorf("remove state: %w", err))
    }
    if len(errs) > 0 { return errors.Join(errs...) }
    return nil
}
```

Two patterns worth internalizing:

1. **Keep going on cleanup failures.** The first `if err != nil { return err }` instinct is wrong here. If the unmount fails, we *still* want to try destroying the cgroup and removing state — otherwise a partial failure leaves more garbage than necessary. Collect all errors, surface them together via `errors.Join`.
2. **Refuse ambiguous intent, opt-in for destructive.** Running container + no `-f` → refuse. This matches Docker exactly. The "force" flag isn't just convenience — it's a signal that the user understood and accepted the consequence.

`★ Insight ─────────────────────────────────────`
`errors.Join` (Go 1.20+) is the right tool for "try N things, report all failures". Before it, people either returned only the first error (masking others) or built ad-hoc multi-error types (boilerplate). The standard library solution is ~3 lines and integrates with `errors.Is`/`errors.As`. One of those small 1.20 additions that quietly changed how I write cleanup code.
`─────────────────────────────────────────────────`

---

### 6i · State lives on tmpfs — the limitation worth naming

Look carefully at `overlay.go`:

```go
if !mounted {
    unix.Mount("tmpfs", containersDir, "tmpfs", 0, "")
}
```

`/var/lib/mydocker/containers/` — which now holds `state.json`, `stdout.log`, `stderr.log`, *and* the overlay upper/work/merged dirs — is **tmpfs-backed**. That means **all container state evaporates on reboot.**

Is this a bug? Not quite — it's a deliberate scope simplification, with a defensible rationale:

- On reboot, the container's *processes* are gone anyway (tmpfs or not).
- The overlay upper/work/merged dirs on tmpfs die with the reboot too, so there's nothing to clean up on the next boot — no "orphaned container" problem.
- Real Docker persists state *and* reconciles stale containers on daemon start. We have no daemon, so "clean slate on reboot" is consistent with our architecture.

The trade-off: `mydocker ps -a` after a reboot will show nothing, even for containers that exited cleanly before. Real Docker would still list them. Worth fixing when we build the daemon (M9).

---

### M6 summary — what each command answers

| Command | Question it answers | Key mechanism |
|---|---|---|
| `run <img>` | Start a container from a local image | `image.Resolve` + auto-pull on `ErrImageNotFound` |
| `run -d` | Start detached (background) | `Setsid` + log redirect + skip `Wait`/`Unmount` |
| `ps` / `ps -a` | What's running / what ever ran? | `state.List` + lazy reconciliation |
| `logs <id>` | What did it print? | Read `stdout.log` (detached only) |
| `stop <id>` | Ask nicely, then force | SIGTERM → poll → SIGKILL, with `ESRCH` tolerance |
| `rm <id>` | Forget this container | `errors.Join` over unmount + cgroup + state cleanup |

The three cross-cutting ideas tying them together:

1. **`(PID, StartTime)` is the process identity** — PIDs alone aren't safe.
2. **Lazy reconciliation** at every read keeps state honest without a reaper daemon.
3. **Atomic writes + split cleanup responsibility** — state surviving past container exit is what enables `ps -a` and `logs`.

---

## Putting it all together — commands across milestones

### `mydocker pull alpine:3.19`

```
[pull] parseRef           → ("library/alpine", "3.19")
[pull] GetManifest(tag)   → Index JSON (multi-arch)  [HTTP × 3: 401, token, retry]
[pull] matchPlatform      → pick linux/<GOARCH> entry
[pull] GetManifest(dgst)  → Manifest JSON            [HTTP × 1, cached token]
[pull] FetchBlob(config)  → /var/lib/mydocker/blobs/sha256_xxx/data   (SHA-256 verified)
[pull] for each layer:
         FetchBlob        → blobs/sha256_yyy/data    (streaming gzip blob)
         ExtractLayer     → layers/sha256_yyy/...    (gunzip → untar → atomic rename)
[pull] SaveImage          → images/library_alpine_3.19/{manifest,config}.json
```

### `mydocker run alpine:3.19 /bin/sh` (foreground)

```
mydocker run --memory 64 --cpu 50 alpine:3.19 /bin/sh
    │
    ▼
[parent] overlay.EnsureRoot()                        ← tmpfs on /var/lib/mydocker/containers/ only
[parent] store.Resolve("alpine:3.19")                ← image dir → manifest → reversed layer digests
           │ (ErrImageNotFound? → store.Pull then Resolve again)
           ▼
[parent] overlay.Mount(id, layers)                   ← stacked rootfs → mergedPath
[parent] cgroup.Create(limits)                       ← mkdir + write memory.max, cpu.max
[parent] exec.Command("/proc/self/exe", "init", ...) ← with CLONE_NEWPID|NEWNS|NEWUTS|NEWIPC
[parent] cg.AddPID(child.Pid)
[parent] state.ReadStartTime(child.Pid)              ← record the (PID, StartTime) identity
[parent] Container.Save()                            ← state.json, atomic write
[parent] signal.Notify(SIGINT/SIGTERM) → forward     ← let the user's Ctrl-C reach the child
[parent] Wait()
             │
             ▼
        [child=PID 1 in ns] sethostname, setupRoot, setupMounts
        [child] exec.Command(user's binary).Start()  ← init stays alive as PID 1
        [child] signal.Notify(SIGTERM/INT/QUIT/HUP/USR1/USR2) → forward to user proc
        [child] Wait() on user proc → os.Exit(code)
             │
             ▼  (user exits / stop / crash)
[parent] state.Status = "exited", ExitCode, FinishedAt
[parent] Container.Save()                            ← final state persisted
[parent] overlay.Unmount(id)                         ← tear down merged (state dir stays!)
[parent] cg.Destroy()
```

### `mydocker run -d alpine:3.19 /bin/sleep 3600` (detached)

```
(same setup through cgroup.AddPID + state.Save)
    │
    ▼
[parent] stdout/stderr → /var/lib/mydocker/containers/<id>/std{out,err}.log
[parent] cmd.SysProcAttr.Setsid = true               ← new session, survives shell SIGHUP
[parent] cmd.Start() → RETURN IMMEDIATELY           ← no Wait, no cgroup.Destroy, no Unmount
[parent] print container ID                          ← like `docker run -d`

(container lives on; `mydocker ps` shows it, `logs <id>` reads the log file)

... eventually ...

[user] mydocker stop <id>
   → state.Find → IsRunning? → SIGTERM → waitForExit(10s) → SIGKILL → reconcileExited

[user] mydocker rm <id>
   → overlay.Unmount + cgroup.Destroy + state.RemoveDir (errors.Join all three)
```

---

## What's next (milestones 7–10)

| # | What | Why it's the logical next step |
|---|------|-------------------------------|
| 7 | **Networking** (veth, bridge, NAT) | The big one. `CLONE_NEWNET` + creating a veth pair + a bridge + iptables NAT so containers can reach the internet. |
| 8 | Volumes | Bind mounts from host into container — trivial *mechanically*, fiddly in UX. |
| 9 | Daemon architecture | Split `mydocker` into CLI + daemon with a Unix socket API — this also solves M6's state-on-reboot problem and the PID 1 zombie-reaping gap. |
| 10 | CLI polish | Cobra, better errors, maybe `logs -f` / `exec` / `inspect`. |

---

## Open questions to sit with

### From M1–M5
- If we forgot the `MS_PRIVATE|MS_REC` line in `setupRoot`, what exactly would break on the host, and when would we notice?
- The gap between `cmd.Start()` and `cg.AddPID()` is a real race. How does `clone3(CLONE_INTO_CGROUP)` close it, and why can't we easily use it from Go's `os/exec`?
- Why does overlayfs want the lowerdir list in *top-most-first* order? (We answered this in `image.Resolve`: lookups resolve top-down and manifests list bottom-up, so we reverse. Make sure you can explain it in your own words.)
- Our `Client` caches a single bearer token. What changes if a user does `mydocker pull alpine && mydocker pull nginx` in one process? (Hint: read the `scope` field of the WWW-Authenticate challenge carefully.)
- `ExtractLayer` skips `.wh.*` whiteout files. When would stacking two pulled layers require *applying* these instead of skipping?

### New to M6
- What goes wrong if we kill the parent `mydocker run` process with `SIGKILL` while the container is alive in foreground mode? Which pieces of state end up inconsistent, and which of our other commands self-heal it?
- Our `init` inside the container only `Wait`s for its direct child. Imagine the user runs `bash -c 'sleep 1000 & exec /bin/foo'`. When `foo` exits, what happens to the `sleep` process, and what's the observable consequence from outside? (Hint: zombies, PID leaks.)
- `stop` polls `IsRunning` every 100ms. What's the alternative using `pidfd_open(2)` + `poll(2)`, and why is it strictly better? Why didn't we reach for it?
- Why did we keep `/var/lib/mydocker/containers/` on tmpfs even though it now holds `state.json` (persistent-looking data)? What specifically would break if we moved it to disk without *also* adding a reconciliation pass on startup?
- `ps` reconciles stale "running" status at read time. What fails if two `mydocker` processes run `ps` simultaneously and both try to `Save()` the same container's state? Is this actually a problem in our use case?
- `rm` doesn't verify that overlay unmount succeeded before removing the state directory. What's the worst case if the unmount fails silently? (Hint: mount point vs state dir are sibling paths.)
