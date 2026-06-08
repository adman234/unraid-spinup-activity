package monitor

/*
	fileactivity-watcher
	Copyright (C) 2025-2026 Derek Kaser

	This program is free software: you can redistribute it and/or modify
	it under the terms of the GNU General Public License as published by
	the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU General Public License for more details.

	You should have received a copy of the GNU General Public License
	along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
)

type openFileCacheEntry struct {
	containerID string
	cachedAt    time.Time
}

// OpenFileResolver recovers the container behind a file event when the kernel
// attributes it to Unraid's user-share FUSE handler (/usr/libexec/unraid/shfs)
// instead of the container that requested the I/O.
//
// On Unraid, a container reading /mnt/user/<share>/... has the request served
// by shfs, which performs the underlying read on /mnt/<disk-or-pool>/<share>/...
// fanotify therefore reports shfs as the PID, and shfs is not in a Docker
// cgroup -- so cgroup-based detection yields nothing. We recover the real client
// by scanning /proc/<pid>/fd for another process holding the same file open.
//
// Matching is done three ways because a container's fd paths are rendered in its
// own mount namespace (e.g. /data/...), not the host path:
//   - exact host path (direct /mnt access from the host),
//   - same inode (direct disk/pool access from inside a container),
//   - path suffix (user-share access, where only the share-relative tail matches).
//
// Scans are cached per file path with a TTL so sustained reads (e.g. streaming a
// media file) only pay the cost once.
type OpenFileResolver struct {
	mu     sync.Mutex
	cache  map[string]openFileCacheEntry
	ttl    time.Duration
	negTTL time.Duration
}

func NewOpenFileResolver() *OpenFileResolver {
	return &OpenFileResolver{
		cache: make(map[string]openFileCacheEntry),
		ttl:   30 * time.Second,
		// Don't hold on to "couldn't find a container" results for long: a
		// short-lived reader (e.g. immich opening a photo) may not have the file
		// open at scan time, but a repeat access moments later might, so re-check
		// soon instead of caching the miss for the full TTL.
		negTTL: 3 * time.Second,
	}
}

// ResolveByOpenFile returns the Docker container ID of a process (other than the
// process that triggered the event) holding filePath open, or "" if none found.
func (r *OpenFileResolver) ResolveByOpenFile(filePath string, excludePID int) string {
	now := time.Now()

	r.mu.Lock()
	if e, ok := r.cache[filePath]; ok {
		ttl := r.ttl
		if e.containerID == "" {
			ttl = r.negTTL
		}

		if now.Sub(e.cachedAt) < ttl {
			r.mu.Unlock()

			return e.containerID
		}
	}
	r.mu.Unlock()

	cid := scanForOpenFile(filePath, excludePID)

	r.mu.Lock()
	r.cache[filePath] = openFileCacheEntry{containerID: cid, cachedAt: now}

	for key, entry := range r.cache {
		if now.Sub(entry.cachedAt) > r.ttl*2 {
			delete(r.cache, key)
		}
	}
	r.mu.Unlock()

	return cid
}

func scanForOpenFile(filePath string, excludePID int) string {
	exactPaths := candidatePaths(filePath)
	suffixes := candidateSuffixes(filePath)
	targetDev, targetIno, haveStat := statDevIno(filePath)

	log.Debug().
		Str("file", filePath).
		Int("exclude_pid", excludePID).
		Interface("exact", keys(exactPaths)).
		Strs("suffixes", suffixes).
		Bool("have_inode", haveStat).
		Msg("open-file scan: looking for container holding this file open")

	procEntries, err := os.ReadDir("/proc")
	if err != nil {
		return ""
	}

	for _, procEntry := range procEntries {
		name := procEntry.Name()
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}

		pid, err := strconv.Atoi(name)
		if err != nil || pid == excludePID {
			continue
		}

		fdDir := "/proc/" + name + "/fd"

		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}

		for _, fd := range fds {
			fdPath := fdDir + "/" + fd.Name()

			target, err := os.Readlink(fdPath)
			if err != nil {
				continue
			}

			if !fdMatches(target, fdPath, exactPaths, suffixes, targetDev, targetIno, haveStat) {
				continue
			}

			cid := getContainerID(pid)

			log.Debug().
				Int("pid", pid).
				Str("fd_target", target).
				Str("container_id", cid).
				Msg("open-file scan: found a process holding the file open")

			// If it's in a container, that's our answer; otherwise keep scanning
			// for a containerized holder (e.g. skip shfs itself).
			if cid != "" {
				return cid
			}
		}
	}

	log.Debug().Str("file", filePath).Msg("open-file scan: no containerized holder found")

	return ""
}

func fdMatches(
	target, fdPath string,
	exactPaths map[string]struct{},
	suffixes []string,
	targetDev, targetIno uint64,
	haveStat bool,
) bool {
	if _, ok := exactPaths[target]; ok {
		return true
	}

	for _, suffix := range suffixes {
		if target == suffix || strings.HasSuffix(target, "/"+suffix) {
			return true
		}
	}

	// Same underlying file (handles direct disk/pool access from inside a
	// container, where the fd path is namespaced but the inode is shared).
	if haveStat {
		if dev, ino, ok := statDevIno(fdPath); ok && dev == targetDev && ino == targetIno {
			return true
		}
	}

	return false
}

// candidatePaths returns the host paths a fd might resolve to for the same file:
// the literal disk/pool path and its /mnt/user (user-share) equivalent.
func candidatePaths(filePath string) map[string]struct{} {
	out := map[string]struct{}{filePath: {}}

	if rel, ok := relUnderMnt(filePath); ok {
		if i := strings.IndexByte(rel, '/'); i > 0 {
			out["/mnt/user/"+rel[i+1:]] = struct{}{}
		}
	}

	return out
}

// candidateSuffixes returns share-relative tails used to match a container's
// namespaced fd path (e.g. /data/torrents/x.mkv) against the host event path
// (/mnt/cache/data/torrents/x.mkv).
func candidateSuffixes(filePath string) []string {
	var out []string

	if rel, ok := relUnderMnt(filePath); ok {
		// "<share>/<rest>" (rel under the disk/pool)
		out = append(out, rel)
		// "<rest>" (rel under the share)
		if i := strings.IndexByte(rel, '/'); i > 0 {
			out = append(out, rel[i+1:])
		}
	}

	return out
}

// relUnderMnt strips a leading "/mnt/<disk-or-pool>/" prefix, returning the rest.
func relUnderMnt(filePath string) (string, bool) {
	const prefix = "/mnt/"
	if !strings.HasPrefix(filePath, prefix) {
		return "", false
	}

	rest := filePath[len(prefix):]

	i := strings.IndexByte(rest, '/')
	if i <= 0 || i+1 >= len(rest) {
		return "", false
	}

	return rest[i+1:], true
}

func statDevIno(path string) (uint64, uint64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, false
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}

	return uint64(st.Dev), uint64(st.Ino), true
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}
