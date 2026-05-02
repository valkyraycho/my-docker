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

After M6 (with orphan reaping, added as M6.5 polish):

```go
// new: init STAYS ALIVE as PID 1 and owns BOTH PID-1 responsibilities:
// (a) signal forwarding, (b) reaping any zombie that re-parents to us.

childCh := make(chan os.Signal, 1)
signal.Notify(childCh, syscall.SIGCHLD)   // armed BEFORE Start to avoid a race
cmd.Start()
directChild := cmd.Process.Pid

fwdCh := make(chan os.Signal, 1)
signal.Notify(fwdCh, SIGTERM, SIGINT, SIGQUIT, SIGHUP, SIGUSR1, SIGUSR2)
go func() { for sig := range fwdCh { _ = cmd.Process.Signal(sig) } }()

os.Exit(reapUntilDirectExits(directChild, childCh))
```

with the reaper itself:

```go
func reapUntilDirectExits(directChild int, childCh <-chan os.Signal) int {
    for range childCh {
        // SIGCHLD coalesces — drain with WNOHANG on every wakeup.
        for {
            var ws syscall.WaitStatus
            pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
            if err != nil || pid <= 0 { break }
            if pid != directChild { continue }     // silently reap orphans

            switch {
            case ws.Exited():   return ws.ExitStatus()
            case ws.Signaled(): return 128 + int(ws.Signal())   // shell convention
            default:            return 1
            }
        }
    }
    return 1
}
```

**Why the change?** In a container with `CLONE_NEWPID`, the first process *is* PID 1 — the kernel's init process from that namespace's perspective. PID 1 has two special responsibilities:

1. **Signal default handlers don't apply.** The kernel refuses to deliver signals whose only handler is the default to PID 1. If init doesn't explicitly register a handler for `SIGTERM`, `kill 1` does *nothing*. That's why `docker stop` on a naive image often seems to hang for 10 seconds then SIGKILL.
2. **Orphan reaping.** When a process's parent dies, its children re-parent to PID 1, which must `wait()` for them when they exit. Without reaping, zombies accumulate until you run out of PIDs (or hit `pids.max`).

`★ Insight ─────────────────────────────────────`
**Why we abandoned `cmd.Wait()`:** Go's `cmd.Wait()` calls `Wait4(specificPID, ...)` — it only reaps the direct child. If we *also* run a `Wait4(-1, ...)` reaper, there's a race: our reaper might grab the direct child first, and then `cmd.Wait()` returns `ECHILD` because the child is already reaped. The clean fix is to do *all* reaping ourselves via `Wait4(-1, WNOHANG)` and ignore `cmd.Wait()` entirely. We track the direct child's PID manually and return its exit code when we see it come through the reaper loop.

**The `128 + signal` convention:** when a process is killed by signal N (not normal `exit()`), the convention is to report exit code `128 + N`. So `SIGKILL=9` → exit 137, `SIGTERM=15` → exit 143. Matches bash, matches Docker, matches every other runtime. It's the reason you've seen "exit 137" before and wondered what 137 meant.

**`SIGCHLD` can coalesce.** If three children die in the same scheduler tick, we get *one* `SIGCHLD`, not three. That's why the inner loop drains with `WNOHANG` until `Wait4` reports "nothing more right now" — otherwise we'd leak zombies whenever siblings died close together.
`─────────────────────────────────────────────────`

**Why registering `SIGCHLD` notification *before* `cmd.Start()` matters:** in theory, the child could exit between `Start()` returning and `signal.Notify()` installing. If that happens, the SIGCHLD hits Go's default (ignored) before we're watching, and our reaper never wakes up — the child's zombie sits there forever. Arming the handler first closes that window.

> **Junior-dev gotcha to internalize:** "PID 1 is special" is not a rule you'll find from reading userspace code — it's kernel behavior. When you inherit a container that "mysteriously doesn't respond to SIGTERM," or accumulates zombies without anyone's code being obviously buggy, the answer is almost always: PID 1 didn't take on its two responsibilities. `tini` is a 500-line C program that exists precisely because Docker images run `node` or `python` as PID 1 without knowing this.

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

## Milestone 7 — Networking (bridge, veth, NAT, DNS)

**The core change:** we added `CLONE_NEWNET` to the clone flags. That one flag creates a brand-new network namespace for the container — and that namespace is *completely empty*. No `eth0`, no `lo`, no routes, no DNS. A process in there can't even ping `127.0.0.1` until we give it a loopback.

So the milestone is a single question in four parts:

> Given an empty netns, how do we give the container (a) an IP, (b) a route to the outside world, (c) source-IP masquerading so return traffic finds it, and (d) DNS to resolve names?

The answer is a four-layer stack, which is exactly what real Docker does:

```
                   The Internet
                        ▲
                        │  (source IP masqueraded by iptables NAT)
                        │
   host's eth0 ─────────┘
                        │
   ┌────────────────────┴─────────────────────┐
   │                HOST NETNS                 │
   │                                           │
   │  mydocker0 (bridge, 172.42.0.1/24) ◄──┐   │
   │                                       │   │
   │    v<id1>  v<id2>  v<id3>  ← attached to bridge (host side of veth)
   │      │      │      │                      │
   │ ═════╪══════╪══════╪════════ namespace boundary ═════
   │      │      │      │                      │
   │    eth0   eth0   eth0  ← renamed from peer side (inside container netns)
   │    .2      .3     .4                      │
   │                                           │
   │  container-1  container-2  container-3    │
   │                                           │
   └───────────────────────────────────────────┘
```

Every container gets its own netns, its own IP, its own `eth0`. They all plug into a shared L2 switch (the bridge) in the host netns, and traffic to the outside world gets source-NAT'd on the way out.

---

### 7a · The `ip` package — persistent IP allocation

Before we can plumb, we need to decide *which* IP each container gets. Our subnet is `172.42.0.0/24` with gateway `172.42.0.1`. That leaves 253 usable IPs (.2 through .254).

```go
// internal/network/ip.go
const (
    subnet    = "172.42.0.0/24"
    gatewayIP = "172.42.0.1"
    allocFile = "/var/lib/mydocker/network/allocated_ips.json"
)

type allocation struct {
    ContainerID string `json:"container_id"`
    IP          string `json:"ip"`
}
```

The allocator is dead simple: enumerate candidate IPs, skip the ones already taken, pick the first free one, persist.

```go
// AllocateIP (simplified)
used := setOf(existingAllocations)
for _, c := range ipRange(subnet) {
    if _, taken := used[c]; !taken {
        picked = c; break
    }
}
allocations = append(allocations, allocation{id, picked})
writeAllocations(allocations)  // temp + rename, like every other persistent write in this project
```

Three small design choices worth naming:

1. **Skip the gateway IP in `ipRange`** — `.1` belongs to the bridge itself, not to containers. Forgetting this causes spectacularly confusing ARP collisions.
2. **Skip `.0` and `.255`** — network address and broadcast. The loop `for i := 1; i < total-1; i++` enforces this.
3. **Temp-write + rename** — same atomicity pattern as M5's blob writes. Prevents a corrupt JSON from killing the next allocation attempt.

`★ Insight ─────────────────────────────────────`
**Why persist allocations to disk at all?** Because we have no daemon. If IPs lived in a `map[string]string` in a running process, every `mydocker run` would start fresh with no knowledge of which IPs are already bound to living containers, and you'd double-assign. The file is how multiple `mydocker` invocations agree on shared state.

**The limitation this creates:** the file persists across reboots, but the kernel's network state doesn't. After a reboot, `allocated_ips.json` still lists IPs as "taken" that no one actually holds. Over time, the file fills up with stale entries and we run out. A production fix would reconcile allocations against actually-running containers (or store per-container in `state.json` and let it die with the container's state dir). Noted as an open question.
`─────────────────────────────────────────────────`

---

### 7b · The bridge (`mydocker0`) — a virtual L2 switch

A **bridge** in Linux is a software implementation of an Ethernet switch: a device that lives in the host kernel, has a MAC address and optional IP, and forwards L2 frames between all devices attached to it. If you think of `eth0` as a cable port on a physical switch, `mydocker0` is the switch itself — except entirely in software.

```go
// internal/network/bridge.go — EnsureBridge (four steps)
func EnsureBridge() error {
    if !bridgeExists() {
        createBridge()       // ip link add mydocker0 type bridge
        assignGatewayIP()    // ip addr add 172.42.0.1/24 dev mydocker0
    }
    bringBridgeUp()          // ip link set mydocker0 up
    enableIPForwarding()     // sysctl -w net.ipv4.ip_forward=1
    return nil
}
```

**Why each step:**

- **`ip link add mydocker0 type bridge`** — creates the kernel object. After this, `ip link show` lists it, but it has no IP and is DOWN.
- **`ip addr add 172.42.0.1/24 dev mydocker0`** — gives the bridge itself an IP in the container subnet. This IP is the **default gateway** for every container. When a container sends a packet to `1.1.1.1`, its route says "send via 172.42.0.1" — the bridge receives it at its host-side interface, and Linux's routing stack takes over from there.
- **`ip link set mydocker0 up`** — activates the interface. A DOWN interface drops everything.
- **`sysctl -w net.ipv4.ip_forward=1`** — flips on routing between interfaces. Without this, the kernel treats itself as an end-host (only processes packets *for* the host's IPs). With it on, the kernel will forward packets between `mydocker0` and the host's external interface (`eth0`, Wi-Fi, etc.). **This is the single setting that turns your laptop into a router for containers.**

**Idempotency:** `bridgeExists` is just `ip link show mydocker0` — exit code 0 means it's there. Checking first avoids the `RTNETLINK answers: File exists` error when we re-run.

---

### 7c · veth pairs — the "Ethernet cable" between namespaces

A **veth pair** is the single most important primitive in Linux container networking. Think of it as a virtual Ethernet cable: two interfaces that are connected to each other such that anything sent into one comes out the other. Crucially, the two ends can live in *different network namespaces*.

```go
// internal/network/veth.go — SetupVeth, the choreography
func SetupVeth(containerID string, pid int, ip string) error {
    host := "v" + containerID   // the end that stays in host netns
    peer := "p" + containerID   // the end that moves into container netns

    createVethPair(host, peer)              // ip link add v<id> type veth peer name p<id>
    movePeerSideIntoNetns(peer, pid)        // ip link set p<id> netns <pid>
    attachHostSideToBridge(host)            // ip link set v<id> master mydocker0
    bringHostSideUp(host)                   // ip link set v<id> up
    configureInsideNetns(pid, peer, ipCIDR) // everything inside the container's netns
}
```

The six-step dance is worth internalizing because it mirrors how *every* container runtime sets up networking:

```
Step 1: create the pair        [host]      [host]
                                v<id> ═══ p<id>

Step 2: move peer into netns   [host]      [container netns]
                                v<id> ═══ p<id>

Step 3: plug v into bridge     [host: bridge]──v<id> ═══ p<id>  [container netns]

Step 4: bring v up             [host: bridge]──v<id>═══ p<id>  [container netns]
                                                 UP

Step 5+: inside container netns, configure p (rename to eth0, set IP, default route)
```

The "configure inside netns" step is done via `nsenter`:

```go
// internal/network/veth.go — configureInsideNetns
nsRun(pid, "ip", "link", "set", peerSide, "name", "eth0")    // rename p<id> → eth0 (convention)
nsRun(pid, "ip", "link", "set", "lo",     "up")              // loopback — needed for 127.0.0.1!
nsRun(pid, "ip", "addr", "add", ipCIDR,   "dev", "eth0")     // assign IP + prefix
nsRun(pid, "ip", "link", "set", "eth0",   "up")              // activate
nsRun(pid, "ip", "route", "add", "default", "via", gatewayIP) // default route → bridge IP
```

Where `nsRun` is:

```go
func nsRun(pid int, cmd string, args ...string) error {
    all := append([]string{"-t", strconv.Itoa(pid), "-n", cmd}, args...)
    return run("nsenter", all...)
}
```

`nsenter -t <pid> -n <cmd>` means "enter the **n**etwork namespace of process `<pid>` and run `<cmd>` there." It works by opening `/proc/<pid>/ns/net` and calling `setns(2)` before executing the command. This is how we can configure the container's network *from outside* — we never enter the container's mount namespace, just its netns.

`★ Insight ─────────────────────────────────────`
**Why bring `lo` up explicitly?** Every other namespace Linux creates (PID, mount, IPC, UTS) comes with sane defaults. A new netns is the odd one out: it's born with nothing, not even loopback. Processes that want to talk to themselves via `127.0.0.1` (which is a huge fraction of real-world software — databases, web servers, test suites) will silently fail until `lo` is up. The one-line `ip link set lo up` is probably the most commonly-forgotten step in hand-rolled container networking.

**Why rename `p<id>` to `eth0`?** Convention. Every program that hard-codes `eth0` (a lot of them, unfortunately) suddenly works. Docker does the same thing. The "proper" solution is to use any interface name and rely on routing, but convention wins.

**Why the default route via the bridge IP?** When a packet in the container wants to leave the 172.42.0.0/24 subnet (say, to `8.8.8.8`), the kernel consults the route table. `default via 172.42.0.1` says "for anything I don't know about, send to the bridge IP." The bridge is in the host netns, so the packet lands in the host kernel, which then re-routes it via the external interface. This is how the container reaches the outside world at the IP layer.
`─────────────────────────────────────────────────`

---

### 7d · NAT via iptables MASQUERADE — making return traffic work

The container has an IP (`172.42.0.5` say) and a route to the gateway. But `172.42.0.0/24` is a **private RFC1918 range** — no router on the public internet knows how to route return packets to it. If we just let the packet out, it'd arrive at some server somewhere, and the reply would vanish into the void.

The fix is **source NAT**, specifically MASQUERADE:

```go
// internal/network/nat.go
iptables -t nat -A POSTROUTING -s 172.42.0.0/24 ! -o mydocker0 -j MASQUERADE
```

Reading this rule piece by piece:

| Piece | Meaning |
|---|---|
| `-t nat` | Work in the NAT table (separate from filter/mangle tables). |
| `-A POSTROUTING` | Append to the POSTROUTING chain — fires just before a packet leaves the host. |
| `-s 172.42.0.0/24` | Match: source IP is in our container subnet. |
| `! -o mydocker0` | Match: going *out* any interface except the bridge itself. |
| `-j MASQUERADE` | Action: rewrite source IP to whatever the outgoing interface's IP is. |

So when container `172.42.0.5` sends a packet destined for `8.8.8.8`:

```
1. Container kernel routes via default gateway 172.42.0.1
2. Packet arrives at mydocker0 (host kernel)
3. Host kernel routes via external interface (e.g., eth0 192.168.1.42)
4. POSTROUTING fires: source 172.42.0.5 → rewritten to 192.168.1.42
5. Packet leaves eth0 with source 192.168.1.42 — a real routable IP
6. 8.8.8.8 replies to 192.168.1.42
7. Host kernel NAT table remembers the mapping, rewrites dst back to 172.42.0.5
8. Packet comes back into mydocker0, into the container
```

**Why `! -o mydocker0`?** Without this, container-to-container traffic within the subnet would *also* get masqueraded, which mangles source IPs when two containers on the same bridge talk to each other. The exclusion keeps bridge-internal traffic untouched and only NATs outbound-to-internet traffic.

**Idempotency via `-C`:** `iptables -C` checks if a rule exists (exit 0) or not (exit 1). `EnsureNAT` tries `-C` first; only if absent does it `-A`. This lets us re-run `mydocker run` without duplicating the rule — each duplicate would slow down every packet by a small amount and accumulate over time.

---

### 7e · DNS — the simplest piece

A container with an IP and a route can reach `8.8.8.8`, but can it resolve `example.com`? Not without a nameserver configured. That's `/etc/resolv.conf`:

```go
// internal/network/dns.go
const resolvConfContents = `nameserver 8.8.8.8
nameserver 1.1.1.1
`

func WriteResolvConf(rootfs string) error {
    os.MkdirAll(filepath.Join(rootfs, "etc"), 0755)
    os.WriteFile(filepath.Join(rootfs, "etc", "resolv.conf"), []byte(resolvConfContents), 0644)
}
```

Two things worth noting:

1. **We write to `rootfs/etc/resolv.conf` from the parent** — which is the merged overlay mount. That write lands in the container's **upperdir** (writable layer), so it overrides whatever the image's base layers had. Perfect — we don't mutate the image.
2. **Hard-coded public resolvers.** Real Docker copies `/etc/resolv.conf` from the host (or runs an embedded DNS server for service discovery). Hard-coding `8.8.8.8`/`1.1.1.1` is the simplest thing that works and is the right call for now.

---

### 7f · The synchronization problem — why we added a sync pipe

Now for the subtle bit. Network setup has to happen in a particular order:

1. Child must be **created with `CLONE_NEWNET`** — gives us a netns.
2. Child must exist (have a PID) before we can `ip link set <veth> netns <pid>`.
3. But the child must **not execute the user's command** until networking is ready — otherwise the workload sees a half-built netns and might bail out ("no network").
4. Parent must configure the cgroup, then network, then release the child.

The race without synchronization:

```
[parent] cmd.Start()  ← child is alive, starts executing init()
[parent] network.Setup() ...
                            ← child could race ahead, mount /proc, exec the workload,
                              and try to use eth0 before it exists
```

The fix is a **sync pipe** (FD 3):

```go
// internal/container/run.go
pipeR, pipeW, err := os.Pipe()
cmd.ExtraFiles = []*os.File{pipeR}    // becomes FD 3 in the child
defer pipeW.Close()

cmd.Start()
pipeR.Close()                          // parent doesn't need the read end anymore

cg.AddPID(...)
state.ReadStartTime(...)
network.Setup(...)
c.Save()

pipeW.Close()                          // signals the child: you may proceed
```

and on the child side:

```go
// internal/container/init.go
func waitForParent() error {
    syncFile := os.NewFile(3, "sync")
    defer syncFile.Close()
    if _, err := io.Copy(io.Discard, syncFile); err != nil {
        return fmt.Errorf("read sync: %w", err)
    }
    return nil
}

func Init(rootfs string, args []string) error {
    waitForParent()                    // FIRST thing init does — block until parent says go
    // ... sethostname, setupRoot, setupMounts, exec workload ...
}
```

**How it works:**

1. Parent creates a pipe. Parent holds both ends.
2. Parent passes the **read end** to the child via `ExtraFiles` → shows up as FD 3 in the child.
3. Child's `waitForParent` does `io.Copy` from FD 3. This **blocks** — no data, no EOF, because parent still holds the write end.
4. Parent does all the setup (cgroup, network, state save).
5. Parent closes the write end. The kernel sees no one holds it → delivers **EOF** on the read side.
6. Child's `io.Copy` returns (it copied zero bytes, got EOF), `waitForParent` returns, and init proceeds.

`★ Insight ─────────────────────────────────────`
**We never write data through the pipe.** We just use the fact that "writer closed" becomes EOF on the reader. This is a classic Unix "gate" idiom — the pipe is used as pure synchronization, not as a data channel. Go's `io.Copy(io.Discard, pipe)` is the idiomatic way: it reads until EOF and discards; the number of bytes is irrelevant.

**Why FD 3 specifically?** When Go's `exec.Cmd` forks, `Stdin`/`Stdout`/`Stderr` occupy FDs 0/1/2 in the child. Entries in `ExtraFiles` are numbered starting at 3. So `ExtraFiles[0]` → FD 3, `ExtraFiles[1]` → FD 4, and so on. The child uses `os.NewFile(3, "sync")` to get an `*os.File` referencing that descriptor.

**What if the parent crashes between Start and pipeW.Close()?** Great question. If the parent dies, the kernel closes *all* its open FDs — including pipeW. The child immediately gets EOF and proceeds. The child will try to run in an un-networked container, then probably crash, but it won't hang forever. Orphan cleanup then happens through the normal "PID 1 dies → kernel teardown of netns" path.
`─────────────────────────────────────────────────`

---

### 7g · `Setup` and `Teardown` — the orchestration layer

These two functions tie everything together and manage cleanup-on-partial-failure:

```go
// internal/network/setup.go
func Setup(containerID, rootfs string, pid int) (string, error) {
    EnsureBridge()
    EnsureNAT()

    ip, _ := AllocateIP(containerID)

    if err := SetupVeth(containerID, pid, ip); err != nil {
        ReleaseIP(containerID)                 // unwind: release the IP we just claimed
        return "", err
    }
    if err := WriteResolvConf(rootfs); err != nil {
        RemoveVeth(containerID); ReleaseIP(containerID)   // unwind further
        return "", err
    }
    return ip, nil
}

func Teardown(containerID string) error {
    var errs []error
    if err := RemoveVeth(containerID); err != nil   { errs = append(errs, ...) }
    if err := ReleaseIP(containerID); err != nil    { errs = append(errs, ...) }
    return errors.Join(errs...)
}
```

Two patterns worth noticing:

1. **Unwind on error in `Setup`.** If `SetupVeth` succeeds but `WriteResolvConf` fails, we *already* claimed an IP and created a veth. Those need to be released, or we leak both. The pattern is ugly (three nested cleanup calls) but correct — each `if err` path unwinds everything that came before it.
2. **`errors.Join` on `Teardown`.** Same as `rm.go` from M6: try everything, report everything. If veth removal fails, we still want to try releasing the IP.

**What `Teardown` intentionally does *not* do:** remove the bridge or flush the NAT rule. The bridge and NAT are **shared infrastructure** across all containers. Tearing them down when one container exits would break every other running container. They're created once by `EnsureBridge`/`EnsureNAT` and kept around forever (well — until reboot; tmpfs isn't involved here, so they actually persist until the next boot).

---

### 7h · Integration into `run.go`

The pieces slot into the sequence like this:

```go
// internal/container/run.go (simplified, focusing on M7 additions)
Cloneflags: CLONE_NEWPID | NEWUTS | NEWNS | NEWIPC | CLONE_NEWNET  // ← added NEWNET

cg.Create(limits)                                     // [M3]
pipeR, pipeW, _ := os.Pipe()                          // [M7] sync pipe
cmd.ExtraFiles = []*os.File{pipeR}

cmd.Start()                                           // child is alive, blocked on FD 3
pipeR.Close()                                         // parent's copy no longer needed

cg.AddPID(cmd.Process.Pid)                            // [M3]
startTime, _ := state.ReadStartTime(...)              // [M6]
ip, _ := network.Setup(id, rootfs, cmd.Process.Pid)   // [M7] ← all the network plumbing
state.Save(containerWithIP)                           // [M6] — now includes IP

pipeW.Close()                                         // [M7] release the child — it proceeds

// ... foreground: Wait(); on exit: state.Save + Teardown + cg.Destroy ...
```

And in `rm.go`:

```go
// internal/container/rm.go — added one more teardown step
overlay.Unmount(c.ID)
cg.Destroy()
state.RemoveDir(c.ID)
network.Teardown(c.ID)    // ← M7 addition: free the IP and delete veth
```

**The `Container` struct gained one field:**

```go
// internal/state/state.go
type Container struct {
    // ...
    IP string `json:"ip,omitempty"`  // ← M7
}
```

So `mydocker ps` can show the container's IP (once we update `ps.go` to display it — a nice small polish task).

---

### 7i · Why shell out to `ip` and `iptables` instead of using netlink?

Every operation in the network package ultimately calls this:

```go
// internal/network/bridge.go — the universal command wrapper
func run(cmd string, args ...string) error {
    c := exec.Command(cmd, args...)
    var stderr bytes.Buffer
    c.Stderr = &stderr
    if err := c.Run(); err != nil {
        return fmt.Errorf("%s %v: %w: %s",
            cmd, args, err, strings.TrimSpace(stderr.String()))
    }
    return nil
}
```

Go has a real netlink library (`vishvananda/netlink`) that talks the rtnetlink protocol directly — the same protocol `ip` uses under the hood. Why didn't we use it?

**For learning:** shelling out is transparent. Every line of `network/veth.go` corresponds to a command you can type and test manually. If it breaks, you debug with `ip link show`, `ip netns exec <id> ip a`, `iptables -t nat -L -n` — actual tools you'll use for the rest of your career. Netlink would hide the semantics behind a Go API.

**For reliability:** `ip` and `iptables` are stable, well-maintained, and ship with every distro. The netlink library has rough edges around newer kernel features, and binding versions can drift.

**What we give up:** performance (each command is a process fork + exec) and granularity (error handling is string-matching on stderr, which is fragile — you can see this in `veth.go`'s `strings.Contains(err.Error(), "does not exist")`). For a learning project, the trade is fine.

`★ Insight ─────────────────────────────────────`
**`exec.Command` with stderr capture is a 10-line pattern that pays forever.** The naive version (`c.Run()` only) gives you `exit status 2` and nothing else. The improved version attaches a `bytes.Buffer` to `Stderr`, runs, and wraps the error with the stderr text. Suddenly every failure message tells you *what actually went wrong*: "RTNETLINK answers: File exists", "Cannot find device 'pabc'", etc. It's the single most valuable hygiene improvement for any Go code that shells out.
`─────────────────────────────────────────────────`

---

### M7 summary — what each file owns

| File | Responsibility | One-line summary |
|---|---|---|
| `ip.go` | IP allocation + persistence | First free IP in the subnet, tracked in a JSON file |
| `bridge.go` | Bridge lifecycle (shared) | Create `mydocker0` once, turn IP forwarding on |
| `veth.go` | Per-container L2 plumbing | Six-step dance: create pair, move into netns, plug into bridge, configure inside |
| `nat.go` | MASQUERADE rule (shared) | One iptables rule so container traffic can reach the internet |
| `dns.go` | `/etc/resolv.conf` | Two hard-coded public resolvers written into the container's rootfs |
| `setup.go` | Orchestration | `Setup` wires all of the above; `Teardown` unwinds the per-container pieces |

The three ideas tying it all together:

1. **Bridge + veth pairs are how every container runtime does L2** — this isn't a mydocker-specific design, it's literally how `docker0` and Docker's container networking work. You can run `ip link show` on a host with Docker and see the same shape: `docker0` bridge, plus a `veth*@ifN` for each running container.
2. **The kernel's routing stack + iptables NAT is what connects a private subnet to the internet** — once you internalize this, Docker's network stack, Kubernetes' pod-to-pod networking, and any VPN you've ever configured all start to feel familiar.
3. **Synchronization between parent and child is almost as important as the networking itself** — the sync pipe is a tiny amount of code that prevents a whole class of subtle bugs where the workload starts before the network is ready.

---

## Milestone 8 — Volumes (bind mounts + named volumes)

**The core change:** users can now keep data *outside* the container's writable overlay upperdir. Two flavors, both exposed via the same `-v src:dst[:ro]` flag:

- **Bind mount** — `"/host/data:/app/data"` — a specific host path is visible inside the container at a specific target path. Great for sharing code into a dev container, exposing config files, sharing secrets.
- **Named volume** — `"pgdata:/var/lib/postgres"` — mydocker owns the storage under `/var/lib/mydocker/volumes/<name>/_data`, and the user just refers to it by name. Great for persistent application data (databases, caches) that needs to outlive a container but doesn't need a specific host path.

Mechanically, both are the *same* operation inside the kernel (a `MS_BIND` mount). The only difference is where the source path comes from — directly from the user for bind mounts, from a managed path for named volumes.

```
┌─ Bind mount ──────────────┐      ┌─ Named volume ─────────────┐
│ /host/data                │      │ /var/lib/mydocker/volumes/ │
│   └── file.txt            │      │   └── pgdata/_data/        │
│       │ (MS_BIND)         │      │       │ (MS_BIND)          │
│       ▼                   │      │       ▼                    │
│ container:/app/data       │      │ container:/var/lib/postgres│
│   └── file.txt            │      │                            │
└───────────────────────────┘      └────────────────────────────┘
```

---

### 8a · The `Spec` data model — kind + source + target + mode

```go
// internal/volume/parse.go
type Kind int
const (
    Bind Kind = iota
    Named
)

type Spec struct {
    Kind     Kind
    Source   string   // host path (Bind) or volume name (Named)
    Target   string   // absolute path inside the container
    ReadOnly bool
}
```

Four fields. The `Kind` enum flattens "bind or named" into one type instead of using two structs plus an interface — a fine choice for something this small. If we ever add more volume types (tmpfs-backed, nfs, …), this becomes a switch in `Mount`. For two variants, no interface is warranted.

---

### 8b · `Parse` — all the validation in one place

The `-v` flag value is free-form text; `Parse` is the chokepoint that turns it into a validated `Spec`:

```go
// internal/volume/parse.go (annotated)
func Parse(s string) (*Spec, error) {
    parts := strings.Split(s, ":")
    if len(parts) != 2 && len(parts) != 3 {
        return nil, fmt.Errorf("invalid volume spec %q: expected src:dst[:mode]", s)
    }
    source, target := parts[0], parts[1]

    // Non-empty, target absolute.
    if source == "" { return nil, errors.New("volume spec: source is empty") }
    if target == "" { return nil, errors.New("volume spec: target is empty") }
    if !strings.HasPrefix(target, "/") {
        return nil, fmt.Errorf("volume spec: target %q must be absolute", target)
    }

    // Optional mode.
    var readOnly bool
    if len(parts) == 3 {
        switch parts[2] {
        case "ro": readOnly = true
        case "rw": readOnly = false
        default:   return nil, fmt.Errorf("mode %q must be 'ro' or 'rw'", parts[2])
        }
    }

    // Kind is inferred from the shape of the source.
    var kind Kind
    if strings.HasPrefix(source, "/") {
        kind = Bind
    } else {
        if strings.Contains(source, "/") {
            return nil, errors.New("named volume must not contain slashes")
        }
        kind = Named
    }

    return &Spec{Kind: kind, Source: source, Target: target, ReadOnly: readOnly}, nil
}
```

Three things worth naming about this function's *shape*:

1. **Kind is inferred, not explicit.** "Starts with `/`" means bind; otherwise named. This matches Docker's convention. Users don't have to say `bind:/host:/container` vs `named:foo:/container` — the path tells you.
2. **Target must be absolute.** Because we're going to `filepath.Join(rootfs, target)` inside `Mount`. A relative target would silently resolve against the current working directory and mount in the wrong place. Validating at parse time means `Mount` never has to worry about it.
3. **Named volumes can't contain `/`.** This is how we separate "user put a typo in a bind path" from "user is naming a volume". `my/volume` isn't a valid name because it'd be ambiguous in our on-disk layout (`volumes/my/volume/_data` vs `volumes/my%2Fvolume/_data`).

`★ Insight ─────────────────────────────────────`
**Parsing functions are the ideal first thing to unit-test in any project** — and M8 is where we finally do it (`parse_test.go`). Why? Three reasons:

1. They're *pure*: input string in, output struct out, no I/O, no syscalls, no side effects. Tests run in microseconds with no fixtures.
2. They're *boundary code*: every weird user input hits `Parse` first. Every bug here becomes a runtime confusion five functions deep. Catching malformed input at the boundary is defense-in-depth.
3. They have *clear specs*: "valid if X, error containing Y if not." Writing these tests forces you to enumerate the negative cases, which forces you to actually think about edge cases.

The test file uses the idiomatic **table-driven pattern**: one slice of structs, each describing `{input, want, wantErr}`, looped with `t.Run(tt.name, ...)`. Adding a new case is one line. This is the Go style for any test function that wants to grow.
`─────────────────────────────────────────────────`

---

### 8c · `EnsureNamed` — the named-volume lifecycle

```go
// internal/volume/volume.go
const volumesDir = "/var/lib/mydocker/volumes"

func EnsureNamed(name string) (string, error) {
    if strings.HasPrefix(name, ".") {
        return "", fmt.Errorf("volume name %q cannot start with '.'", name)
    }
    dataDir := NamedPath(name)              // /var/lib/mydocker/volumes/<name>/_data

    if err := os.MkdirAll(dataDir, 0755); err != nil {
        return "", fmt.Errorf("mkdir %s: %w", dataDir, err)
    }
    return dataDir, nil
}

func NamedPath(name string) string {
    return filepath.Join(volumesDir, name, "_data")
}
```

The `_data` subdir is deliberate: it leaves room for sibling metadata in the future (a `opt.json` for volume driver options, labels, owner, etc. — the way Docker does). Today we don't have metadata, but the layout is ready for it.

**Forbidding `.`-prefixed names** blocks two classes of mischief:
- `.` and `..` — let the user accidentally escape the volumes dir via `filepath.Join`.
- Hidden names that resemble filesystem detritus (`.DS_Store`, `.bashrc`).

**Idempotent creation.** `EnsureNamed` always `MkdirAll`s — whether the volume existed before this call or not, the data dir exists when we return. That's why `Mount` can call it unconditionally on every container start, no bookkeeping needed.

---

### 8d · `Mount` — the three-line syscall + the readonly gotcha

```go
// internal/volume/mount.go
func Mount(spec *Spec, rootfs string) error {
    var source string
    switch spec.Kind {
    case Bind:  source = spec.Source
    case Named: source, _ = EnsureNamed(spec.Source)
    }

    target := filepath.Join(rootfs, spec.Target)
    os.MkdirAll(target, 0755)

    // Step 1: bind-mount source onto target. Both must exist.
    if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
        return fmt.Errorf("mount %s to %s: %w", source, target, err)
    }

    // Step 2: if readonly requested, REMOUNT as readonly.
    if spec.ReadOnly {
        if err := unix.Mount("", target, "",
            unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
            _ = unix.Unmount(target, unix.MNT_DETACH)
            return fmt.Errorf("remount %s for readonly: %w", target, err)
        }
    }
    return nil
}
```

This function is where every interesting Linux-mount gotcha lives. Four in particular:

**1. The readonly remount dance.** This one trips up every container-runtime author exactly once. You'd expect this to work:

```go
unix.Mount(source, target, "", unix.MS_BIND|unix.MS_RDONLY, "")    // DOESN'T WORK
```

It doesn't. The kernel **silently ignores `MS_RDONLY`** when you pass it together with `MS_BIND` on the first bind. The mount succeeds, but it's read-write. This is a long-standing kernel quirk — the rationale is that `MS_BIND` creates a *new reference* to an existing superblock, and you can only change the readonly-ness of a mount by remounting. So:

- **First call:** bind-mount read-write. Mount exists.
- **Second call:** remount the *mount point* with `MS_BIND|MS_REMOUNT|MS_RDONLY`. Note `source=""` — we're not binding anything new, just changing attributes on the mount that's already there.

If the remount fails (rare, but possible — e.g., the underlying FS is mounted readonly and we tried to remount rw over it), we tear down the first bind to avoid leaving a leaked read-write mount.

**2. `MkdirAll(target, 0755)` before mounting.** The target *must* exist as a directory (or file, for file binds). If the target doesn't exist, `mount(2)` returns `ENOENT`. Creating it lives in the container's overlay upperdir, so no host files are touched and the layer below is unaffected.

**3. Target exists in the overlay merged view.** When we `filepath.Join(rootfs, spec.Target)`, `rootfs` is the overlay's merged mount (e.g., `/var/lib/mydocker/containers/<id>/merged`). So we're carving a mount point *inside the overlay*, which has an important consequence for ordering — see the next section.

**4. `MNT_DETACH` on the cleanup path.** If the remount fails, we unmount with `MNT_DETACH` (lazy unmount) rather than the default sync unmount. `MNT_DETACH` removes the mount from the filesystem tree immediately but waits for any in-use references to drain before actually releasing the superblock. For cleanup paths it's the safer choice — a plain unmount can fail with `EBUSY` if anything has the path open.

---

### 8e · Why volumes mount *before* the child starts — the CLONE_NEWNS interaction

Look at the sequence in `run.go`:

```go
// internal/container/run.go (simplified order)
cg.Create(...)

for _, spec := range opts.Volumes {           // ← volumes mounted in PARENT's mount ns
    volume.Mount(spec, opts.Rootfs)           //   onto merged path BEFORE child exists
}

pipe, _, _ := os.Pipe()
cmd.ExtraFiles = []*os.File{pipe}
cmd.Start()                                   // ← child clones with CLONE_NEWNS
                                              //   inherits parent's mount tree as its initial state

// ... cgroup, state, network setup ...

pipeW.Close()                                 // ← child unblocks, runs setupRoot → pivot_root
```

And in the child (`init.go → setupRoot`):

```go
unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, "")   // make ns private
unix.Mount(rootfs, rootfs, "", unix.MS_BIND|unix.MS_REC, "") // bind rootfs to itself
unix.PivotRoot(rootfs, oldRoot)                             // swap roots
```

Now trace what happens to a volume mount:

```
Parent's mount ns (right after volume.Mount):
  /var/lib/mydocker/containers/<id>/merged/          ← overlay
  /var/lib/mydocker/containers/<id>/merged/app/data/ ← bind mount from /host/data

─── child is cloned with CLONE_NEWNS ───
Child's mount ns (identical initial state):
  /var/lib/mydocker/containers/<id>/merged/
  /var/lib/mydocker/containers/<id>/merged/app/data/ ← still here, shared initially

─── child runs setupRoot ───
  Mount("", "/", MS_PRIVATE|MS_REC)   ← detach propagation
  Mount(rootfs, rootfs, MS_BIND|MS_REC) ← rootfs becomes its own mount point (needed for pivot_root)
                                          the MS_REC makes this RECURSIVE — it pulls in all
                                          submounts, including /app/data
  PivotRoot(rootfs, oldRoot)

Child after pivot_root:
  /app/data/                          ← (was rootfs/app/data) still bind-mounted to /host/data
```

**The crucial detail is `MS_REC` on the rootfs bind.** Without it, `pivot_root` would succeed but the sub-bind-mounts would be left behind in the "old root" (which we then `MNT_DETACH` away — killing the volume mounts). With `MS_REC`, the sub-mounts are *recursively* carried along into the pivoted root.

If you've ever wondered why M2's `setupRoot` has `MS_BIND|MS_REC` rather than just `MS_BIND` — this is why. It's been ready for M8 since M2.

`★ Insight ─────────────────────────────────────`
**Mounting in the parent before clone is the simpler choice.** The alternative is: pass the `Spec` list into the child via stdin/env/file, and have the child mount inside its own namespace after `pivot_root`. That works too, and Docker actually does it this way. The advantage: the mounts are scoped to the child's namespace from the start, and they automatically disappear when the namespace is destroyed (no cleanup needed on crash).

We chose parent-side mounting because our volume cleanup path runs on `rm`, and we have the container ID + rootfs path handy there anyway. The trade-off: if the parent crashes between `volume.Mount` and `cmd.Start`, the mounts leak on the host. Mitigation would be cleaning them up in an error path in `Run`.
`─────────────────────────────────────────────────`

---

### 8f · Unwind on partial failure — a pattern you've now seen three times

From `run.go`:

```go
var mountedSoFar []*volume.Spec
for _, spec := range opts.Volumes {
    if err := volume.Mount(spec, opts.Rootfs); err != nil {
        for _, prev := range mountedSoFar {    // undo everything we mounted up to this point
            _ = volume.Unmount(prev, opts.Rootfs)
        }
        cg.Destroy()                           // and undo the cgroup we created before the loop
        return fmt.Errorf("mount volume %s:%s: %w", spec.Source, spec.Target, err)
    }
    mountedSoFar = append(mountedSoFar, spec)
}
```

This is the third time we've seen this general shape:

| Milestone | Where | The "N things" |
|---|---|---|
| M5 | `image.Pull` | N blob downloads — if layer 3 of 5 fails, the already-extracted ones stay (they're content-addressed, safe to keep) |
| M7 | `network.Setup` | IP alloc, veth, resolv.conf — if any step fails, unwind the earlier ones |
| M8 | `volume.Mount` loop | If volume N fails, unmount volumes 1..N-1 |

The general pattern: when you do a sequence of operations that each claim resources, keep a list of what you succeeded at, and on the first failure, walk the list backwards releasing each. In Go, the explicit slice (`mountedSoFar`) is idiomatic; in languages with deferred cleanup (C++ RAII, Rust's `Drop`), you'd write a helper type with a destructor. Both solve the same problem.

The trap to avoid: doing the cleanup inside a `defer` without also disarming it on success. Too easy to accidentally unmount everything *after* a successful run.

---

### 8g · Repeatable flags — how `-v` takes multiple values

```go
// cmd/mydocker/repeatable_flags.go
type stringSliceFlag []string

func (s *stringSliceFlag) String() string  { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error {
    *s = append(*s, v)
    return nil
}
```

Go's stdlib `flag` package only supports one value per flag by default. Passing `-v a -v b` with a plain `fs.String` would keep the *last* value and throw away the first. To accept repetition, we implement the **`flag.Value` interface** (two methods: `String` and `Set`), and each invocation of `-v` calls `Set`, appending to the slice.

Used like:

```go
var volumeSpecs stringSliceFlag
fs.Var(&volumeSpecs, "v", "volume mount (repeatable): src:dst[:ro]")
```

After `fs.Parse`, `volumeSpecs` is a `[]string` with every `-v` value in order.

This is the smallest piece of M8 but probably the most broadly reusable pattern in the whole project — any time you need `-flag x -flag y -flag z` with stdlib flags, this is the three lines of code.

---

### 8h · Integration — where volumes slot into the lifecycle

**`RunOptions` grows a field:**

```go
// internal/container/run.go
type RunOptions struct {
    // ... existing ...
    Volumes []*volume.Spec
}
```

**Mount order in `Run`:** volumes are mounted *after* the cgroup is created but *before* the child is started (so `CLONE_NEWNS` carries them into the child).

**`Container` state persists the specs:**

```go
// internal/state/state.go
type Container struct {
    // ... existing ...
    Volumes []*volume.Spec `json:"volumes,omitempty"`
}
```

This is important for `rm` — when we remove a container, we need to know *which* bind mounts to unmount from its overlay. Without persisting, we'd have to either unmount everything in the merged path (overbroad) or require the user to repeat the `-v` flags to `rm` (terrible UX).

**`rm.go` unmounts volumes before unmounting the overlay:**

```go
// internal/container/rm.go
for _, spec := range c.Volumes {
    volume.Unmount(spec, overlay.MergedPath(c.ID))   // ← new helper exposing the merged path
}
overlay.Unmount(c.ID)
```

**Why this order matters:** the overlay can't be unmounted while there's still a bind mount *on top of it*. `unmount(2)` returns `EBUSY`. Unmounting the volumes first peels them off, then the overlay can unmount cleanly.

**`overlay.MergedPath(id)` is new:** a small helper that exposes the merged path without re-mounting. Previously `overlay.Mount` was the only way to get that path; now we need it at `rm` time too.

```go
// internal/overlay/overlay.go
func MergedPath(containerID string) string {
    return filepath.Join(containersDir, containerID, "merged")
}
```

Tiny function, real separation-of-concerns: the *path format* is now defined in one place.

---

### M8 summary — what each file owns

| File | Responsibility | Key idea |
|---|---|---|
| `parse.go` | Turn `-v` text into a validated `Spec` | Kind inferred from shape of source; target must be absolute |
| `parse_test.go` | Lock down parser behavior | First table-driven tests in the project — the template for all future parse tests |
| `volume.go` | Named-volume lifecycle | `/var/lib/mydocker/volumes/<name>/_data/`, idempotent `EnsureNamed` |
| `mount.go` | Do the actual bind mount | `MS_BIND`, then `MS_REMOUNT|MS_RDONLY` if readonly |
| `cmd/mydocker/repeatable_flags.go` | Let `-v` appear multiple times | `flag.Value` impl: `Set` appends to a slice |
| `overlay/overlay.go` | Expose `MergedPath` | So `rm` can unmount volumes without re-mounting the overlay |
| `container/run.go` | Mount volumes, save them in state | Unwind-on-partial-failure pattern |
| `container/rm.go` | Unmount volumes before overlay | Order matters: submounts must unmount first |
| `state/state.go` | Persist the spec list | So `rm` can reverse what `run` did |

The three big ideas tying it together:

1. **`MS_BIND` is the universal primitive** — bind mounts are how Docker does volumes, how `pivot_root` gets a valid new root, how host paths are injected into containers. Every volume operation in every container runtime is some decoration on top of bind mounts.
2. **The readonly-remount quirk is a kernel interface reality**, not an mydocker quirk — if you ever write infrastructure that touches mounts, you'll meet it again.
3. **Parent-side mounts + `MS_REC` on the rootfs bind = volumes inherited by the child** — this is why `setupRoot` has `MS_REC` from day 1 (M2). The design decision paid dividends six milestones later.

---

## Milestone 10 — CLI polish + env vars + port publishing + anonymous volumes + inspect

M10 is the "make it usable" milestone. Five changes, each small on its own; together they turn a working-but-raw runtime into something shaped like the `docker` you use every day. One of them — **port publishing** — required a multi-round debugging session that taught deep Linux networking. We'll walk through that in detail because the bugs are the kind you'll meet again, in other systems, and recognize immediately.

### What shipped

```
cmd/mydocker/
├── main.go        → cobra root; owns exit-code propagation
├── run.go         → runCmd (all the flags)
├── pull.go pullCmd      ps.go psCmd          logs.go logsCmd
├── stop.go stopCmd      rm.go rmCmd          inspect.go inspectCmd  (NEW)
└── init.go        → initCmd (Hidden + DisableFlagParsing)

internal/network/ports.go                 (NEW: ParsePortSpec, PublishPorts, UnpublishPorts)
internal/network/nat.go  + bridge.go     (UPDATED: hairpin MASQUERADE, route_localnet sysctl)
internal/volume/parse.go                  (UPDATED: anonymous volume branch)
internal/state/state.go                   (UPDATED: +IP, +Ports)
internal/container/run.go  + rm.go       (UPDATED: env, ports, teardown signatures)
```

### User-visible additions

```bash
mydocker --help                         # cobra-generated
mydocker run -e KEY=VAL ...             # explicit env var
mydocker run -e KEY ...                 # inherit KEY from host env
mydocker run -p 8080:80 ...             # publish TCP port
mydocker run -v /container/only/path    # anonymous volume (named with random suffix)
mydocker inspect <id>                   # full state JSON
```

---

### 10a · Migrating to cobra — why it actually matters

The old dispatch was:

```go
switch os.Args[1] {
case "run":   runCommand(os.Args[2:])
case "pull":  pullCommand(os.Args[2:])
// ... etc
}
```

Every command lived in `main.go`. Adding `inspect` would have meant editing `main.go` (grow the switch), adding another `Command` function beside all the others, and hand-rolling `--help`. Not fatal, but the file would keep growing.

Cobra flips the shape. One command, one file:

```go
// cmd/mydocker/inspect.go — the entire new command, 20 lines
var inspectCmd = &cobra.Command{
    Use:   "inspect <id>",
    Short: "Display detailed information about a container",
    Args:  cobra.ExactArgs(1),
    RunE: func(cmd *cobra.Command, args []string) error {
        c, err := state.Find(args[0])
        if err != nil { return err }
        b, _ := json.MarshalIndent(c, "", "  ")
        fmt.Fprintln(os.Stdout, string(b))
        return nil
    },
}
```

And `main.go` becomes a three-line registration + an error-handling harness:

```go
// cmd/mydocker/main.go
rootCmd.AddCommand(runCmd, initCmd, pullCmd, psCmd, logsCmd, stopCmd, rmCmd, inspectCmd)

for _, c := range rootCmd.Commands() {
    c.SilenceErrors = true    // main owns all printing
}

err := rootCmd.Execute()
if err == nil { return }

var ee *exec.ExitError
if errors.As(err, &ee) {
    os.Exit(ee.ExitCode())   // container exit code propagates
}
fmt.Fprintln(os.Stderr, "Error:", err)
os.Exit(1)
```

`★ Insight ─────────────────────────────────────`
**Four cobra idioms worth internalizing, because they generalize to every cobra CLI you'll ever write:**

1. **`RunE` returning `error`, `main` owning `os.Exit`.** Commands never call `os.Exit` themselves — they return errors up. `main()` decides policy (print + exit code). This is *exactly* the same separation as `http.Handler` (return result, let the server write it) or `testing.T` (return pass/fail, let the framework print). It's a pattern that shows up everywhere.
2. **`SilenceUsage: true` on the root + `SilenceErrors: true` on each child.** Cobra's default behavior is to print usage on every error (because the user "clearly made a flag mistake") and also print the error. For a real CLI that's *horrible* — you type `mydocker run foo bar` with a typo and get a wall of help text mixed with your error. These two flags kill both defaults; `main()` prints errors deliberately.
3. **`DisableFlagParsing: true` on `init`.** The internal `init` subcommand takes `rootfs cmd arg1 arg2 ...` and the args may legitimately include things like `-e` that cobra would try to parse as flags. Disabling parsing hands the whole tail to `RunE` as raw `args`. Pair with `Hidden: true` so it doesn't show in `--help`.
4. **`f.SetInterspersed(false)` on `run`.** This is the most important flag-handling decision in the file. Without it, `mydocker run alpine:3.19 sh -c 'echo $FOO'` parses `-c` as a flag to `mydocker`, not to `sh`. Turning interspersing off means: "once you see the first positional (`alpine:3.19`), everything after is positional." Users tripped over this exact issue in the migration, with the error `unknown shorthand flag: 'c' in -c`.
`─────────────────────────────────────────────────`

There's one other structural change worth noting. `stringSliceFlag` from M8 is gone — cobra's `StringArrayVarP` replaces it:

```go
f.StringArrayVarP(&runVolumes, "volume", "v", nil, "...")   // repeatable, accumulates values
f.StringArrayVarP(&runEnv,     "env",    "e", nil, "...")
f.StringArrayVarP(&runPorts,   "publish","p", nil, "...")
```

Three repeatable flags, three lines, uniform shape. The old hand-rolled `flag.Value` implementation worked but is obsolete now.

---

### 10b · Environment variables — two-form parser in 6 lines

```go
// cmd/mydocker/run.go
for _, e := range runEnv {
    if strings.Contains(e, "=") {
        envs = append(envs, e)                       // -e FOO=bar → "FOO=bar"
    } else {
        if val, ok := os.LookupEnv(e); ok {
            envs = append(envs, e+"="+val)            // -e FOO → read from host, inject "FOO=<host value>"
        }
        // missing-from-host: silently skipped (matches docker's behavior)
    }
}
```

And the injection in `run.go`:

```go
// internal/container/run.go
cmd.Env = append(os.Environ(), opts.Env...)
```

This is a design choice worth naming: `cmd.Env` starts with the *entire host environment* and appends our opts on top. `-e` values win on duplicate keys (Go's `exec.Command` respects last-write-wins in the env slice). Docker, by contrast, gives containers a minimal default environment (`PATH`, `HOSTNAME`, `HOME`). We inherit everything.

**Consequence:** if your shell exports `AWS_SECRET_ACCESS_KEY`, every container you run sees it. For a learning tool that's fine; for production it'd be a leak. One of the small things a real daemon (M9) would clean up.

**Flow end-to-end:** host shell → `mydocker` CLI → spawns `init` with `cmd.Env = host + opts.Env` → `init` runs user workload with `cmd.Env = os.Environ()` (inherits its own) → workload sees the whole merged set. Three processes deep, env flows through.

---

### 10c · Port publishing — and the three bugs we caught along the way

This is the hardest piece of M10 and the most educational. The surface feature is `-p 8080:80` — publish container port 80 on host port 8080. The mechanism is two iptables DNAT rules. But getting it to actually deliver a packet to the container took three iterations of debugging, each one teaching a different Linux-networking concept.

#### The design — two chains, two destinations

```go
// internal/network/ports.go — iptablesRule
args := []string{"-t", "nat", action, chain,
    "-p", spec.Protocol,
    "--dport", strconv.Itoa(spec.HostPort)}
if outLoopback {
    args = append(args, "-o", "lo")
}
args = append(args, "-j", "DNAT", "--to-destination",
    fmt.Sprintf("%s:%d", containerIP, spec.ContainerPort))
```

`PublishPorts` installs **two** rules per port:

| Chain | Matches packets from | Why |
|---|---|---|
| **PREROUTING** | External machines arriving on `eth0` (or whatever) | Packet enters from outside → DNAT rewrites dst before routing → routed to container |
| **OUTPUT** | Host's own processes hitting `localhost:8080` | Locally-originated packets never hit PREROUTING; they hit OUTPUT. Need a separate rule. |

Both rewrite the destination from `:8080` to `<containerIP>:80`. The `-o lo` qualifier on the OUTPUT rule scopes it to loopback-destined traffic only — we don't want to mangle every outbound packet from every host process.

```go
// internal/network/ports.go — PublishPorts with unwind-on-partial-failure
var installed []*PortSpec
for _, spec := range specs {
    runIPTablesAppend("PREROUTING", containerIP, spec, false)
    runIPTablesAppend("OUTPUT",     containerIP, spec, true)   // outLoopback=true
    installed = append(installed, spec)
}
```

Same unwind pattern you've seen in M5/M7/M8 — keep a list, walk it back on failure.

#### The debugging journey — what we actually hit

The initial test went like this:

```bash
# Start a container publishing a port, with a sleep keeping it alive
mydocker run -d -p 8080:80 alpine:3.19 sleep 300

# Curl it from the host
curl -v http://localhost:8080/
```

We expected "Connection refused" (packet reaches container, nobody listening on port 80, TCP RST sent back — success for our purposes, proving end-to-end delivery). What we got was a **hang**. The iptables counter in PREROUTING/OUTPUT showed our DNAT rule was matching (counter > 0), but curl timed out.

**Hang vs refused is a crucial diagnostic distinction**: refused means the packet got there and came back. Hang means the packet got *eaten* somewhere silently — either going or coming back. That shifted us into a different failure mode.

---

#### Bug #1: the martian packet — `route_localnet=0`

Trace what happens to `curl localhost:8080` on the host, step by step:

```
1. curl creates TCP packet:  src=127.0.0.1, dst=127.0.0.1:8080
2. Kernel routes it:          dst=127.0.0.1 → outgoing interface = lo
3. OUTPUT chain (nat) fires:  our DNAT rule matches, rewrites dst to 172.42.0.2:80
4. Kernel re-evaluates route: dst=172.42.0.2 → outgoing interface = mydocker0
5. Packet now has:            src=127.0.0.1, dst=172.42.0.2, outgoing = mydocker0
6. Kernel's martian filter:   "127.0.0.1 source on mydocker0? That's suspicious."
   → DROPS the packet
```

A **martian** is a packet whose source IP is impossible for the interface it's traversing. `127.0.0.0/8` is reserved for `lo`; seeing it on any other interface is classically a spoofing attempt, so the kernel drops it by default. That's a security feature going back decades.

For DNAT'd loopback traffic to reach the container, we need the kernel's explicit permission to treat `127.0.0.1` as a legitimate source on `mydocker0`. That permission is a per-interface sysctl:

```go
// internal/network/bridge.go — the fix
func enableRouteLocalnet() error {
    return run("sysctl", "-w", fmt.Sprintf("net.ipv4.conf.%s.route_localnet=1",
        bridgeName))
}
```

Called from `EnsureBridge`, after the bridge is up (the sysctl only exists once the interface does).

`★ Insight ─────────────────────────────────────`
**This is the exact problem real Docker solved in its early port-publishing work.** If you `cat /proc/sys/net/ipv4/conf/docker0/route_localnet` on any host with Docker, it reads `1`. Docker sets it automatically on its bridge. Reading their source once, it's one function call you'd easily miss. We rediscovered it by hitting the same wall.

**Broader lesson:** when your iptables rule counter bumps but the packet doesn't arrive, the kernel's netfilter hooks are firing but something *past netfilter* is dropping it. Candidates are: routing-stack drops (martian, rp_filter, no route), conntrack misconfiguration, or FORWARD policy. Count your counters in every chain along the path — the hop where the counter *doesn't* bump is where the packet died.
`─────────────────────────────────────────────────`

---

#### Bug #2: per-namespace `127.0.0.1` — the hairpin problem

We added `route_localnet=1`, re-ran curl — and it **still hung**. The martian filter now permitted the packet to leave the host, but something *else* was eating it. Time to think harder.

The key realization: `127.0.0.1` is a **per-network-namespace** address. Every netns has its own loopback, its own `127.0.0.1`. The host's `127.0.0.1` and the container's `127.0.0.1` are literally different interfaces that happen to share a name — like "room 5" in two different hotels.

Trace the packet again, this time from the container's view:

```
Container receives:  src=127.0.0.1, dst=172.42.0.2:80
Container processes it (or in our test, has nothing listening, sends RST)
Container's kernel responds:  src=172.42.0.2:80, dst=127.0.0.1
Container's kernel routes:    dst=127.0.0.1 → outgoing interface = lo  (container's OWN lo!)
Reply never leaves the container.
```

The reply went to the container's own loopback interface and died there. It never came back out via `eth0` → bridge → host. The host's curl waited forever.

**The fix: hairpin SNAT.** We rewrite the source IP of the DNAT'd packet to something routable from the container's namespace — the bridge's IP (`172.42.0.1`). When the container sees `src=172.42.0.1`, it replies to the bridge's IP, which routes via its default gateway (which *is* the bridge), back out through `eth0`, back to the host.

```go
// internal/network/nat.go — EnsureNAT (post-fix)
// Rule 1 (always existed, from M7): masquerade OUTBOUND container traffic to the internet
run("iptables", "-t", "nat", "-A", "POSTROUTING",
    "-s", subnet, "!", "-o", bridgeName, "-j", "MASQUERADE")

// Rule 2 (M10 fix): hairpin — masquerade INBOUND-via-DNAT packets going INTO the bridge
ensureRule("POSTROUTING",
    "-o", bridgeName, "!", "-s", subnet, "-j", "MASQUERADE")
```

Reading the new rule piece by piece:
- `-o mydocker0` — **leaving** via our bridge (so it's about to enter the container)
- `! -s 172.42.0.0/24` — NOT sourced from within the container subnet (we don't want to touch container-to-container traffic)
- `-j MASQUERADE` — rewrite source to the outgoing interface's IP — `172.42.0.1`, the bridge's IP

Net effect: host-originated packets (`src=127.0.0.1` or `src=192.168.x.x`, whatever) get their source rewritten to `172.42.0.1` as they enter the bridge. The container sees `src=172.42.0.1`, replies to `172.42.0.1`, reply routes out naturally. Conntrack reverses everything on the way back (undoing both the DNAT and the SNAT), and curl receives a normal "Connection refused."

```
Host netns                                   Container netns
──────────────────────────                   ────────────────────
curl: src=127.0.0.1, dst=127.0.0.1:8080

  OUTPUT (nat) DNAT
   → dst=172.42.0.2:80

  POSTROUTING (nat) MASQUERADE ← NEW M10 RULE
   → src=172.42.0.1

  packet on mydocker0: src=172.42.0.1, dst=172.42.0.2:80 ─────►  receives packet
                                                                 sends RST:
                                                                   src=172.42.0.2:80,
                                                                   dst=172.42.0.1
                                                               ◄───── packet on eth0

  conntrack un-rewrites:
    SNAT reversed: dst=172.42.0.1 → dst=127.0.0.1
    DNAT reversed: src=172.42.0.2:80 → src=127.0.0.1:8080
  curl sees: "Connection refused from 127.0.0.1:8080" — as expected.
```

**Bonus discovery:** hairpin MASQUERADE *also* fixes the martian problem, because the source IP gets rewritten to `172.42.0.1` *before* the packet leaves via `mydocker0`. The packet is no longer a martian at that point. In principle we could drop `route_localnet=1`, but we kept it as defense-in-depth — two fixes for the same class of bug is cheap insurance.

---

#### Bug #3 (the red herring): alpine's minimal busybox

Before we got to the real bugs, we spent two rounds trying to run a server inside the container as a target for curl. `busybox httpd` failed with "applet not found" — alpine's default busybox doesn't include `httpd` (it's in `busybox-extras`, which isn't pre-installed).

The moral is smaller but useful: **stop testing the thing you haven't changed.** Port publishing is a network-layer concern. We don't need a server to prove the network path; `sleep 300` + "expect Connection refused" is strictly better as a test because it isolates the network layer from the application layer. "Connection refused" proves the packet reached the container and its kernel replied — no dependency on what's installed in the image. Once we switched to that test, we immediately caught the real hang-vs-refused signal.

`★ Insight ─────────────────────────────────────`
**Diagnostic tests should minimize their surface area.** If you're testing port forwarding, use the simplest possible workload inside the container. If you're testing file mounts, use `cat`. If you're testing DNS, use `nslookup`. Every extra dependency you add to a test ("let's install httpd and serve a file") is an extra place where failure can hide the actual signal you care about. The inverse corollary: when a test fails in an unexpected way, ask whether the failure is in the thing you built or in the test harness you wrapped around it. Two rounds of "httpd: applet not found" was all test-harness noise.
`─────────────────────────────────────────────────`

---

#### `Teardown` signature change

Unpublishing ports requires knowing the container's IP (it's in the DNAT rule's `--to-destination`), so `Teardown` grew two arguments:

```go
// internal/network/setup.go
func Teardown(containerID string, ports []*PortSpec, ip string) error {
    UnpublishPorts(ip, ports)
    RemoveVeth(containerID)
    ReleaseIP(containerID)
    // errors.Join the lot
}
```

And `rm.go` reads them back from the persisted state:

```go
// internal/container/rm.go
network.Teardown(c.ID, c.Ports, c.IP)
```

This is exactly why `Container.Ports` and `Container.IP` had to be persisted — without them, we'd have no way to know which iptables rules to remove at `rm` time. Same mechanical reason as M8's `c.Volumes` persistence.

---

### 10d · Anonymous volumes — one line of Parse, big UX win

M8 parsed `src:dst[:mode]`. Users said "I want persistence but I don't care about naming." So we added a third shape: just `/container/path`.

```go
// internal/volume/parse.go
func Parse(s string) (*Spec, error) {
    if !strings.Contains(s, ":") {
        if !strings.HasPrefix(s, "/") {
            return nil, fmt.Errorf("volume spec %q: expected src:dst[:mode] or /container/path", s)
        }
        return &Spec{
            Kind:   Named,
            Source: generateAnonymousName(),   // "anon_" + random 8 bytes
            Target: s,
            ReadOnly: false,
        }, nil
    }
    // ... existing src:dst[:mode] path unchanged ...
}

func generateAnonymousName() string {
    b := make([]byte, 8)
    _, _ = rand.Read(b)
    return "anon_" + hex.EncodeToString(b)
}
```

Usage:

```bash
mydocker run -v /var/lib/postgres alpine:3.19 sh
# creates /var/lib/mydocker/volumes/anon_deadbeefcafebabe/_data/ under the hood
# mounts it at /var/lib/postgres inside the container
```

**Why `anon_` prefix?** Named volumes can't contain slashes, and we wanted a reserved prefix that never clashes with user names. `anon_` is recognizable in `ls /var/lib/mydocker/volumes/` and hints at lifecycle — these are safe to prune.

The rest of the code didn't change. Once a `Spec` exists, the machinery in M8 handles it identically to an explicit named volume. This is the kind of small addition that comes *cheap* when the underlying abstraction is well-shaped, and comes *expensive* when it isn't. We got lucky — our M8 `Kind Bind|Named` enum already covered "named volume that I happen to have generated."

---

### 10e · `inspect` — the payoff for persisting state well

```go
// cmd/mydocker/inspect.go — the entire command
var inspectCmd = &cobra.Command{
    Use:  "inspect <id>",
    Short: "Display detailed information about a container",
    Args:  cobra.ExactArgs(1),
    RunE: func(cmd *cobra.Command, args []string) error {
        c, err := state.Find(args[0])
        if err != nil { return err }
        b, _ := json.MarshalIndent(c, "", "  ")
        fmt.Fprintln(os.Stdout, string(b))
        return nil
    },
}
```

Four effective lines. Load the state, marshal as indented JSON, print.

This is a victory lap for M6's design. We persisted everything the lifecycle commands needed. `inspect` is just exposing that persistence. Every field that shows up (`id`, `image`, `layers`, `pid`, `start_time`, `status`, `exit_code`, `created_at`/`started_at`/`finished_at`, `ip`, `volumes`, `ports`) was added one milestone at a time. None of them required changes here.

> **Design principle this makes tangible:** persist more than you think you need. Data you recorded but never read is cheap; data you needed but didn't record is expensive. `inspect` is the "didn't know I'd want this someday" command that tells you whether your past-self made the right call.

---

### M10 summary — what each piece ships

| Piece | File(s) | What you type | What happens |
|---|---|---|---|
| Cobra migration | `cmd/mydocker/*.go` | `mydocker --help` | Standard CLI shape, command-per-file, `RunE → error → main` error flow |
| Environment vars | `run.go` in container + cmd | `-e FOO=bar`, `-e FOO` | `cmd.Env = append(os.Environ(), opts.Env...)` injects KEYs |
| Port publishing | `network/ports.go`, `nat.go`, `bridge.go` | `-p 8080:80` | PREROUTING + OUTPUT DNAT rules + hairpin MASQUERADE + route_localnet |
| Anonymous volumes | `volume/parse.go` | `-v /container/path` | Auto-generated `anon_xxx` named volume |
| `inspect` | `cmd/mydocker/inspect.go` | `mydocker inspect <id>` | Marshal state as JSON |

**Three enduring ideas from M10:**

1. **Debugging network drops by counter-walk.** When a packet goes missing, check the counter on every iptables chain it *should* traverse. The first chain whose counter doesn't increment is where the packet died. This single technique would have caught route_localnet martian drops in minutes if we'd known to look.
2. **Per-namespace addresses are their own failure mode.** `127.0.0.1` inside a container is not the host's `127.0.0.1`. This intuition extends: `lo` is per-ns, routing tables are per-ns, iptables rules are per-ns. Thinking "same IP string = same thing" is the source of an entire class of container-networking bugs.
3. **Cobra's command-per-file + `RunE` + `SilenceErrors` is the scalable shape for Go CLIs.** Once you learn it, every new subcommand is a 10-line file; every error path is `main()`'s responsibility; every `--help` is free. The migration paid for itself inside M10 alone — `inspect` was a fifteen-line drop-in.

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
[parent] for each -v spec:                           ← [M8] volume mounts (+ M10 anonymous form)
           volume.Mount(spec, mergedPath)            ← MS_BIND onto merged (+ MS_REMOUNT|MS_RDONLY if :ro)
[parent] cmd.Env = append(os.Environ(), opts.Env)    ← [M10] inject -e KEY=VAL into child
[parent] os.Pipe() → cmd.ExtraFiles = {pipeR}        ← [M7] sync pipe as FD 3 in child
[parent] exec.Command("/proc/self/exe", "init", ...) ← with CLONE_NEWPID|NEWNS|NEWUTS|NEWIPC|NEWNET
[parent] cg.AddPID(child.Pid)
[parent] state.ReadStartTime(child.Pid)              ← record the (PID, StartTime) identity
[parent] network.Setup(id, rootfs, child.Pid, ports) ← [M7+M10] bridge+veth+NAT+DNS+ports
           │
           │   EnsureBridge   → ip link add mydocker0 + IP + up + ip_forward=1 + [M10] route_localnet=1
           │   EnsureNAT      → iptables POSTROUTING MASQUERADE (outbound)
           │                  + [M10] POSTROUTING hairpin MASQUERADE (inbound-via-DNAT)
           │   AllocateIP     → pick first free 172.42.0.N
           │   SetupVeth      → create pair, move peer into child's netns, configure eth0
           │   WriteResolvConf → drop 8.8.8.8/1.1.1.1 into rootfs/etc/resolv.conf
           │   PublishPorts   → [M10] for each -p: iptables DNAT in PREROUTING + OUTPUT
           ▼
[parent] Container.Save() (with IP)                  ← state.json, atomic write
[parent] pipeW.Close()                               ← [M7] EOF on FD 3 — child unblocks
[parent] signal.Notify(SIGINT/SIGTERM) → forward     ← let the user's Ctrl-C reach the child
[parent] Wait()
             │
             ▼
        [child=PID 1 in ns] waitForParent (blocks on FD 3 until EOF)
        [child] sethostname, setupRoot, setupMounts
        [child] exec.Command(user's binary).Start()  ← init stays alive as PID 1
        [child] signal.Notify(SIGCHLD) → Wait4(-1, WNOHANG) loop   ← zombie reaping
        [child] signal.Notify(SIGTERM/INT/QUIT/HUP/USR1/USR2) → forward to user proc
        [child] when direct child exits → os.Exit(128+sig OR exit code)
             │
             ▼  (user exits / stop / crash)
[parent] state.Status = "exited", ExitCode, FinishedAt
[parent] Container.Save()                            ← final state persisted
[parent] network.Teardown(id, ports, ip)             ← [M7+M10] unpublish ports, remove veth, release IP
[parent] overlay.Unmount(id)                         ← tear down merged (state dir stays!)
[parent] cg.Destroy()
```

> Note: in the foreground path above, volume mounts are torn down lazily on `mydocker rm`,
> not on container exit — same as the overlay and state dir. `rm` unmounts volumes first,
> then the overlay (sub-mounts block parent unmount otherwise).

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
   → for spec in c.Volumes: volume.Unmount(spec, MergedPath(id))            ← [M8] first
   → overlay.Unmount + cgroup.Destroy + state.RemoveDir
   → network.Teardown(c.ID, c.Ports, c.IP)                                  ← [M7+M10] unpublish ports too
   (all errors.Join'd)
```

---

## Where we landed

The original 10-milestone plan is complete **except for M9 (daemon architecture)**, which we deliberately skipped for M10 — it's a structural overhaul, not a feature, and M10's CLI polish was higher-leverage for a learning project. The deferred work, in rough priority order:

| # | What | Why it's still open |
|---|------|-------------------------------|
| 9 | **Daemon architecture** | Split `mydocker` into CLI + daemon with a Unix socket API. Fixes the cluster of "eventually-consistent" gaps: state survives reboot with daemon-startup reconciliation, stale IP allocations get cleaned up, and the `mydocker run` parent process no longer needs to keep running for detached containers (today it exits after `cmd.Start()`, which is fine but means no one is watching for child death — `ps` has to reconcile lazily). |
| — | `docker exec` | Running a second command inside an existing container. Implementation-wise: `nsenter` into the target's PID + mount + net namespaces, inherit the same cgroup. A 50-line subcommand on top of what we already have. |
| — | `docker logs -f` | Follow mode. Currently `logs` just `io.Copy`s to EOF. For `-f` we'd either `inotify` the log file or poll. |
| — | `volume ls` / `volume prune` | List everything under `/var/lib/mydocker/volumes/`, prune anything not referenced by a live container's state. |
| — | UDP port publishing | One-line change: parse `8080:80/udp` and set `Protocol: "udp"` in the iptables rules. Our `PortSpec.Protocol` field already exists. |
| — | `ps` with IP + PORTS columns | Data's already in `state.Container`; just needs display code. |

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
- Our init now reaps orphans via `Wait4(-1, WNOHANG)` on every `SIGCHLD`. Trace through what happens if the *direct child* (workload) dies first, while an orphan is still running: who kills the orphan, and what's the exit code `mydocker` reports? Now reverse it — orphan dies first, then workload. Same answer or different? (Hint: the PID-namespace teardown rule we met in section 6d.)
- `stop` polls `IsRunning` every 100ms. What's the alternative using `pidfd_open(2)` + `poll(2)`, and why is it strictly better? Why didn't we reach for it?
- Why did we keep `/var/lib/mydocker/containers/` on tmpfs even though it now holds `state.json` (persistent-looking data)? What specifically would break if we moved it to disk without *also* adding a reconciliation pass on startup?
- `ps` reconciles stale "running" status at read time. What fails if two `mydocker` processes run `ps` simultaneously and both try to `Save()` the same container's state? Is this actually a problem in our use case?
- `rm` doesn't verify that overlay unmount succeeded before removing the state directory. What's the worst case if the unmount fails silently? (Hint: mount point vs state dir are sibling paths.)

### New to M7
- `allocated_ips.json` persists to real disk, but `mydocker0`, veth pairs, and iptables rules live only in the kernel — which gets wiped on reboot. What's the observable user-visible failure after, say, 254 reboots (each with one short-lived container), and what's the cheapest fix that doesn't require a daemon? (Hint: there are two different fixes; one reconciles at allocation time, the other relocates the file.)
- If two `mydocker run` invocations race — both read `allocated_ips.json` before either writes back — they can hand out the same IP to two containers. Walk through what happens: does the second container fail immediately, fail on first packet, or silently corrupt the first container's traffic? What's the usual fix, and why didn't we bother?
- We close the sync pipe *after* `state.Save()`. What failure mode are we protecting against by making the child wait that long — versus releasing it right after `network.Setup`?
- Trace a TCP connection from container `172.42.0.5` to `1.1.1.1:443`, listing every table/chain/interface it touches on the way out and every rule that rewrites the packet on the way back. Which of those steps would break if we forgot to flip `net.ipv4.ip_forward=1`?
- `iptables -t nat -A POSTROUTING ... ! -o mydocker0 -j MASQUERADE` excludes traffic going back out via the bridge. Why exactly — what specifically breaks if you remove the `! -o mydocker0` and just MASQUERADE everything from the subnet?
- We use `nsenter -t <pid> -n` to configure the container's interface from outside. What goes wrong if the container's PID 1 dies between `cmd.Start()` and our last `nsenter` call? (Hint: what happens to a netns when all its members have exited?)
- DNS hard-codes `8.8.8.8`/`1.1.1.1`. If the host is on a corporate network that blocks external DNS but offers its own resolver at `10.0.0.53`, every container breaks. What's the minimum-viable fix — and why does the "just copy the host's `/etc/resolv.conf`" answer have a subtle pitfall? (Hint: `127.0.0.53` systemd-resolved entries.)

### New to M8
- Imagine a user runs `mydocker run -v /etc:/host-etc:ro alpine sh`, then inside the container does `touch /host-etc/foo`. What happens, and which of the two layers of defense (`MS_RDONLY`, the overlay being read-write) is actually doing the work? Now imagine they do `mount -o remount,rw /host-etc` from inside — does that work? (Hint: look up "locked mount" in the kernel docs.)
- We mount volumes *in the parent's namespace before `CLONE_NEWNS`* and rely on `MS_REC` in `setupRoot`'s bind-mount of rootfs to carry them through `pivot_root`. If you accidentally drop the `MS_REC`, what exactly do you observe inside the container — the volume mount missing, or something weirder?
- `volume.Mount` calls `os.MkdirAll(target, 0755)` before mounting. That target lives inside the overlay merged view, so the `mkdir` writes to the overlay upperdir. When the container exits but the state dir is kept (as M6 decided), what happens on `mydocker rm` — does the `mkdir`'d directory get cleaned up, leaked in the upperdir, or something else? Trace through `overlay.Unmount` + `state.RemoveDir` to find out.
- `Spec` is persisted to `state.json` via `json.Marshal`. `Kind` is an `int` enum (`Bind = 0`, `Named = 1`). What happens if you later reorder the enum constants — say, add `Tmpfs` at position 0, pushing `Bind` to 1? What does this tell you about serializing enums?
- If the same source path is bind-mounted into two containers simultaneously (say `-v /var/log:/logs` in both), what coordination exists between them? What happens if one container's process writes a file while the other is reading it? (There's no mydocker-specific answer here — the question is purely about Linux bind mount semantics.)
- Named volume directories under `/var/lib/mydocker/volumes/` are never cleaned up — even after the last container that used them is `rm`'d. Compare this to how containers/blobs work. What *should* the policy be, and what command would the user run to trigger it? (Hint: `docker volume prune`.)

### New to M10
- Walk through the *complete* packet path for `curl http://localhost:8080 → container:80`. Name every chain (at minimum: OUTPUT-nat, POSTROUTING-nat, FORWARD-filter, and the reverse path with conntrack un-rewriting) and which rule of ours fires at each one. If any of the three fixes (route_localnet=1, hairpin MASQUERADE, the OUTPUT DNAT chain) were missing, which specific hop would drop the packet?
- We install *two* DNAT rules per port (PREROUTING + OUTPUT). Why isn't PREROUTING enough? What class of traffic does it miss, and why does that class not hit PREROUTING? (Hint: think about where the packet enters the kernel — from an interface, or from a local process?)
- The hairpin MASQUERADE rule uses `! -s 172.42.0.0/24` to exclude container-sourced traffic from being masqueraded on entry to the bridge. What specifically would break if we removed that exclusion — container A curling container B directly, say?
- `PublishPorts` fails if the host port is already in use — but `iptables -A` doesn't check for that. Where does the conflict actually manifest: at rule-install time, at first-packet time, or somewhere else? And how would you build a proper "port already in use" check into the CLI path before `iptables -A`?
- Our `cmd.Env = append(os.Environ(), opts.Env...)` leaks *everything* from the parent shell into the container, including secrets. Write the minimum fix to pass only `PATH`, `HOME`, `HOSTNAME`, and explicitly-listed `-e` values. What's the UX trade-off — what will break that "just worked" before?
- `SetInterspersed(false)` on `run` means `mydocker run alpine:3.19 sh -c 'echo hi' -e FOO=bar` passes `-e FOO=bar` to `sh`, not to `mydocker`. That's what we want for the `-c` case. But now how does a user express `mydocker run -e FOO=bar alpine:3.19 sh`? (Answer: flags before the image ref.) Is that intuition robust? Try `mydocker run alpine:3.19 -e FOO=bar sh` in your head — what does it do?
- When a detached container exits on its own (the supervisor-process case), nothing updates its `state.json` until the *next* `ps` or `stop`/`rm`. If you then `inspect <id>`, what does it show — and is that accurate? Who'd be responsible for making it accurate in real time? (The answer points directly at why M9's daemon matters.)
- `inspect` dumps the raw `Container` struct as JSON, including fields like `pid` and `start_time`. Those are correct while the container is running but meaningless after exit (PID may have been reused). Should `inspect` hide them after exit, or is "display raw truth" the better policy? Compare to what `docker inspect` does.
