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
	"time"
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
// by scanning /proc/<pid>/fd for another process that currently holds the same
// file open, and reading that process's container cgroup.
//
// Scans are cached per file path with a TTL so sustained reads (e.g. streaming a
// media file) only pay the cost once.
type OpenFileResolver struct {
	mu    sync.Mutex
	cache map[string]openFileCacheEntry
	ttl   time.Duration
}

func NewOpenFileResolver() *OpenFileResolver {
	return &OpenFileResolver{
		cache: make(map[string]openFileCacheEntry),
		ttl:   30 * time.Second,
	}
}

// ResolveByOpenFile returns the Docker container ID of a process (other than the
// process that triggered the event) holding filePath open, or "" if none is found.
func (r *OpenFileResolver) ResolveByOpenFile(filePath string, excludePID int) string {
	now := time.Now()

	r.mu.Lock()
	if e, ok := r.cache[filePath]; ok && now.Sub(e.cachedAt) < r.ttl {
		r.mu.Unlock()

		return e.containerID
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
	candidates := candidatePaths(filePath)

	procEntries, err := os.ReadDir("/proc")
	if err != nil {
		return ""
	}

	for _, procEntry := range procEntries {
		if !procEntry.IsDir() {
			continue
		}

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
			target, err := os.Readlink(fdDir + "/" + fd.Name())
			if err != nil {
				continue
			}

			if _, match := candidates[target]; !match {
				continue
			}

			// This process has the file open. If it's in a container, that's our
			// answer; otherwise keep scanning for a containerized holder.
			if cid := getContainerID(pid); cid != "" {
				return cid
			}
		}
	}

	return ""
}

// candidatePaths returns the host paths a container's file descriptor might
// resolve to for the same underlying file: the literal disk/pool path from the
// event and its /mnt/user (user-share) equivalent.
func candidatePaths(filePath string) map[string]struct{} {
	out := map[string]struct{}{filePath: {}}

	const prefix = "/mnt/"
	if strings.HasPrefix(filePath, prefix) {
		rest := filePath[len(prefix):]
		// rest is "<disk-or-pool>/<share>/<...>"; swap the disk/pool for "user".
		if i := strings.IndexByte(rest, '/'); i > 0 {
			out["/mnt/user/"+rest[i+1:]] = struct{}{}
		}
	}

	return out
}
