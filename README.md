# Spinup Activity

[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE)

**Spinup Activity** is an Unraid plugin that shows recent file activity (open / read / write / modify) per
array disk, pool/cache, and Unassigned Device, so you can work out **which Docker container is keeping a disk
spun up**.

It is a fork of the excellent
[**File Activity** plugin by Derek Kaser (dkaser)](https://github.com/dkaser/unraid-fileactivity) — all of
the heavy lifting (the fanotify watcher, container detection, packaging) comes from that project. Please
star and support the original.

## What this fork changes

The upstream plugin already records the container responsible for each event, but in day-to-day use two
things made it hard to answer "what is spinning up my disk?":

1. **The same file is listed once per event.** A container reading a file repeatedly produced hundreds of
   near-identical rows, burying the signal.
2. **The container wasn't front-and-centre**, and there was no easy way to see the *worst* offender at a
   glance.

Spinup Activity addresses both with display-side changes only (the watcher binary is unchanged):

- **Aggregated, de-duplicated rows.** Identical activity — same container, file, and action — is collapsed
  into a **single row with an occurrence count** plus first/last-seen times. No more 200 copies of the same
  path.
- **Container & count promoted.** The table leads with **Count** and **Container**, and sorts by **Count
  (highest first)** by default, so the container hammering a disk is the first thing you see.

Everything else — settings, exclusions, reads vs. writes, supported disks — behaves exactly like upstream.

## Credit & license

- Original plugin: **File Activity** by Derek Kaser — <https://github.com/dkaser/unraid-fileactivity>
- Portions also derive from the original Unraid File Activity work by Dan Landon and Lime Technology.
- Licensed under the **GNU GPL v3**, same as the upstream project. See [LICENSE](LICENSE).

> Note: this is a *lightweight* fork. To keep it easy to track upstream, the internal plugin slug, file
> paths, and Go module path are left as `file.activity` / `unraid-fileactivity`; only the user-facing name
> and the activity table were changed. As a result it installs in place of (not alongside) the original
> File Activity plugin.
