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

## Putting it all together — the two commands' lives

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

### `mydocker run <layer> /bin/sh`

```
mydocker run --memory 64 --cpu 50 <layer-digest> /bin/sh
    │
    ▼
[parent] overlay.EnsureRoot()                        ← tmpfs on /var/lib/mydocker/containers/ only
[parent] cgroup.Create(limits)                       ← mkdir + write memory.max, cpu.max
[parent] exec.Command("/proc/self/exe", "init", ...) ← with CLONE_NEWPID|NEWNS|NEWUTS|NEWIPC
[parent] cg.AddPID(child.Pid)                        ← cage the child
[parent] Wait()
             │
             ▼
        [child] sethostname("my-docker")
        [child] setupRoot(rootfs)                    ← bind + pivot_root + detach old
        [child] setupMounts()                        ← /proc /dev /sys + mknod
        [child] execve("/bin/sh")                    ← child IS now /bin/sh
             │
             ▼  (user types `exit`)
        (child dies → parent unblocks → defers run)
[parent] overlay.Unmount(id)                         ← tear down merged + cleanup dir
[parent] cg.Destroy()                                ← rmdir the cgroup
```

---

## What's next (milestones 6–10)

| # | What | Why it's the logical next step |
|---|------|-------------------------------|
| 5.5 | **Teach `run` to take image refs** | Currently `run` takes a layer *digest* as a positional. Next: resolve `alpine:3.19` → read `images/library_alpine_3.19/manifest.json` → pass the full layer list into `overlay.Mount`. |
| 6 | **Container lifecycle** (`ps`, `stop`, `rm`) | Requires persisting container metadata — a small state store on disk. We already have an on-disk layout convention to imitate. |
| 7 | **Networking** (veth, bridge, NAT) | The big one. `CLONE_NEWNET` + creating a veth pair + a bridge + iptables NAT. |
| 8 | Volumes | Bind mounts from host into container — trivial *mechanically*, fiddly in UX. |
| 9 | Daemon architecture | Split `mydocker` into CLI + daemon with a Unix socket API — mirrors dockerd/docker. |
| 10 | CLI polish | Cobra, better errors, etc. |

---

## Open questions to sit with

- If we forgot the `MS_PRIVATE|MS_REC` line in `setupRoot`, what exactly would break on the host, and when would we notice?
- The gap between `cmd.Start()` and `cg.AddPID()` is a real race. How does `clone3(CLONE_INTO_CGROUP)` close it, and why can't we easily use it from Go's `os/exec`?
- Why does overlayfs want the lowerdir list in *top-most-first* order? (Hint: think about how lookups resolve when the same path exists in two layers.)
- Our `Client` caches a single bearer token. What changes if a user does `mydocker pull alpine && mydocker pull nginx` in one process? (Hint: read the `scope` field of the WWW-Authenticate challenge carefully.)
- `ExtractLayer` skips `.wh.*` whiteout files, but a real extraction-and-stack flow would need to *apply* them — translating "delete this path" markers into overlayfs's own whiteout convention. Where in the pipeline would that translation belong?
- `pull.go` currently throws away the layer ordering when it calls `ExtractLayer` (each layer goes into its own dir). When we wire `run` to take image refs, which *order* must we pass to `overlay.Mount([]string)`, and how does that relate to the order in `manifest.Layers`?
