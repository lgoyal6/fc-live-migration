# AI usage

The challenge invites documenting how AI was used. I (Laksh) built this
pair-programming with an AI agent (Claude, in an agentic CLI). This is an
honest account of the division of labour.

## What the AI did well

- **Source reconnaissance.** Before writing the patch, the AI read the
  Firecracker v1.16.1 source to establish exactly where the "must be paused"
  requirement is enforced (the vCPU `SaveState` handler, not the snapshot
  API) and to confirm `KVM_GET_DIRTY_LOG` is safe on a running VM. That
  reading is what made the `DiffLive` patch a targeted ~250-line change
  rather than a guess.
- **Boilerplate and glue.** The Firecracker API client, the sparse-extent
  wire protocol, the gratuitous-ARP frame construction, the SSE plumbing, and
  the Docker/Lima scaffolding were largely AI-drafted from a clear spec.
- **Measurement discipline.** The AI pushed for client-observed rather than
  self-reported blackout, and for separating brownout from blackout, which is
  what kept the numbers honest.

## What needed human correction / judgement

- **The convergence bug.** The first budget implementation skipped the KVM
  dirty-log fetch while draining the backlog, to save TLB flushes. That
  starved the drain of new dirty information and it never converged - 160+
  rounds shipping the full budget forever. Diagnosing it required watching
  the per-round dirty sizes, not just trusting that "it migrated." The fix
  (fetch every round) was the opposite of the original optimization.
- **The dirty-bitmap reset race.** The naïve `DiffLive` reused upstream
  `dump_dirty`, which resets the VMM dirty bitmap on success - correct when
  paused, silent memory corruption when live. Catching this needed reasoning
  about which thread does VMM-side guest writes, not code generation.
- **Reading the environment honestly.** Much iteration went into telling
  apart "the mechanism is slow" from "the laptop is jittery." The conclusion  - 
  that the client-side numbers are dominated by a nested-virtualization jitter
  floor, and the agent-side measurement is the trustworthy one - is a
  judgement call about *what to measure and believe*, informed by an ambient
  no-migration probe that showed ~25 ms gaps at rest.
- **Scope calls.** Deciding to ship the brownout as documented rather than
  chase a background-dump-thread rewrite, and to record the definitive numbers
  on bare metal rather than fight nested virt, were human decisions about
  where the effort was worth it.

## How to read the result with this in mind

The code is mine to defend line by line; the AI accelerated the mechanical
parts and the source archaeology, and the non-obvious bugs above are the ones
worth asking me about in an interview.
