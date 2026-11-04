# 0024. Keep workload output in the store, shipped by agents, and say what's missing

- **Status:** Proposed (roadmap step 14; built for macos-native and sim; docker next)
- **Date:** 2026-11-04
- **Deciders:** Krishnanand
- **Builds on:** ADR-0015 (logs and transparency: what's collected, for whom, 14 days), ADR-0016 (the
  agent protocol), ADR-0017 (native drivers)

## Context

ADR-0015 decided that a workload's owner can read its output (stdout and stderr), follow it live, and
find it in the export, for 14 days, and that nothing is collected from inside VMs. It left open how
output gets from a machine to the reader without the API ever talking to a runtime (rule 1). For
Solo and Lab:

- **Agents already write it.** macos-native sends each workload's stdout and stderr to
  `logs/stdout.log` and `logs/stderr.log`, which the agent writes and the workload's sandbox can't
  touch.
- **Output carries secrets.** A notebook server prints its URL with `?token=`; a library may print a
  URL with a key in it.
- **Output is unbounded.** A loop that prints can write gigabytes a day, and Solo's store is a SQLite
  file on a laptop.
- **Agents aren't trusted with policy** (ADR-0016). An agent mustn't be able to put output into
  another tenant's workload, or another machine's.

## Decision

**Agents ship output with a second protocol call, `Output`**, beside `Sync`. `Sync` is a long poll
about desired state; output needs to arrive within a second, and mustn't make state changes wait. In
Solo it's a function call; over the network (Phase 2) it will be `POST /agent/v1/output`.

- **Drivers keep output in files that only grow** and expose them through `agent.OutputReader`:
  bytes from an offset, the file's size, and a *source* (an identity that changes when the file is
  replaced; macos-native uses the inode). The agent never follows a symlink to read them.
- **The agent sends whole lines**, every second: output is cut only between lines, and a line without
  its newline waits up to two seconds for the rest, so a line, and any credential in it, arrives whole.
  Only a line longer than a whole chunk goes in pieces.
- **Removing a workload waits for its output.** Removing deletes its files, so the agent stops it,
  sends everything left, and only then removes it. If that can't all be sent (the control plane is
  unreachable), the workload stays, its status says it's sending its last output, and removal is
  tried again on the next sync. One that won't stop is removed regardless.
- **Offsets make it exactly once.** The control plane answers each chunk with how far it has the
  stream, and the agent sends from there, so a restart on either side neither repeats nor drops
  output. A chunk that starts past that point means output was lost on the machine: the gap is counted,
  and a note from Einsora marks the place in the output (`... was lost here`). A new source, or a file
  that shrank, starts the stream over.
- **One request carries at most 1 MiB**, in chunks of at most 256 KiB.

**The control plane keeps output in the store** (`workload_output`, and `workload_output_state` for
each stream's position and counts), and never trusts the agent with whose it is:

- Output is accepted only for workloads placed on the machine sending it, in its region, and stored
  under the workload's tenant from the store. Anything else is ignored, and the agent is told to stop
  sending it.
- On the way in, output becomes valid UTF-8 without NULs, and credentials in URIs are replaced with
  `REDACTED` (`redact.CountSecrets`, the same rules as everywhere else). Each chunk records how many,
  and the export lists them. Everything else is kept as the workload printed it.
- Each workload's output is numbered from 1, and readers keep their place by that number. A number
  across all workloads would let anyone see how much output everyone else's workloads produce.
- Where a stream starts over (a new file: a new run, or another machine), a note says so.
- **Limits, said out loud:** output is kept for `EINSORA_OUTPUT_RETENTION` (14 days), and each
  workload keeps at most `EINSORA_OUTPUT_MAX_MIB` (64 MiB, and no more than 256, since a workload's
  export holds all of it); beyond it, its oldest output is removed, down to nine tenths, a bounded
  amount per append. Bytes lost, trimmed, and expired are counted per stream, and every reader is
  told: the API's notes, the CLI, the dashboard, and the export.
- The region's controller, while it leads, removes expired output every hour.

**Readers get it from the store only:**

- `GET /v1/workloads/{id}/logs?after=<id>&follow=true`: chunks in order, `next` to read on from, and
  `notes` for what isn't collected or was lost or removed. With `follow=true` and nothing new, it waits
  up to 20 seconds, reading the store once a second (as often as agents send). Another tenant's workload is 404; a deleted one's output stays
  readable until it expires.
- `einsora logs <workload> [-f] [-json]`: escape sequences are removed before printing, so output
  can't drive the terminal.
- The dashboard's **Output** section refreshes while the workload runs, and shows the most recent
  1 MiB; it says when it leaves earlier output out, which **Download full log** has.
- The export's `output_logs` has all of it that's kept, with each stream's counts.

**Not collected, and said so:** the inside of VMs (ADR-0015), and runtimes that don't send output
yet: the Kubernetes backend (Loki, Phase 3) and, until roadmap step 14b, the docker driver. Their
readers get a note saying so, never an empty page.

## Consequences

- Solo needs nothing new to run: no log service, no extra process.
- A workload's last output arrives before it's removed: removing it waits until its output is sent.
  While the control plane is unreachable, a workload being deleted stays on its machine.
- Output is the owner's to read, and so is a workload's own token when it prints one, but it's
  redacted anyway: logs get pasted into issues and chats.
- Redaction can miss a credential split across two pieces of output: one in a line longer than
  256 KiB, or in a line printed in parts more than two seconds apart. That's the cost of showing a
  prompt that has no newline yet, rather than holding output back indefinitely; the docs say so.
- A workload that truncates its own output file starts its stream over, so an agent restart in
  between could repeat some output. macos-native's sandbox can't write the file, so this needs a
  driver whose workloads can.
- The per-workload limit bounds the store; it doesn't bound the driver's files on the machine, which
  grow until the workload is removed. Rotating them is future work.
- When sharing arrives (ADR-0014), output from guests on community hosts must be limited to the
  driver's own status lines (ADR-0015); the hub is where that check goes. A host's agent could also
  send made-up output for a guest's workload on its machine, as it could misreport anything else
  about it; hosts can see guests' data anyway (ADR-0014). The dashboard should label output from a
  machine the reader doesn't own.
- Following output costs the store one small read a second per reader; there's no limit on readers
  per tenant yet.

## Alternatives considered

- **Output in `Sync`:** a long poll would hold output back for up to 20 seconds, and a big report would
  slow state changes.
- **The API reading the agent's files, or asking the agent:** breaks rule 1 and would need the machine
  to be reachable from the API.
- **Loki (or another log service) in Solo:** a second service to install, run, and back up on every
  laptop, for what a table holds. The Datacenter profile uses Loki, read through the same API (Phase 3).
- **No size limit, only retention:** one chatty workload could fill a laptop's disk in a day.
- **Server-sent events or WebSockets for follow:** a long poll works through every proxy and the
  existing auth, and the CLI and dashboard need nothing else.
- **Storing output unredacted:** exports and screenshots of logs get shared; ADR-0015's rule is that
  only secrets are removed, and never silently, and that applies here too.
