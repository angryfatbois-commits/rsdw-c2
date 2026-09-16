# Tick measurement design

Issue 3 adds measured tick rate and execution-duration percentiles to the existing telemetry change. The producer belongs in the preloaded rsdw_api library. Unreal and shipped game files remain unchanged.

## Selected producer

Two independent design proposals compared native counter observation with a startup tick wrapper. Cross-review selected the wrapper. No native duration history was found, so polling an engine counter cannot satisfy the percentile requirement. Reviewers used the inherited model, so this comparison has reduced model diversity.

The domain value is a snapshot with an explicit status and an optional coherent measurement. A bounded ring stores completed calls. A prior completion anchors each window. The rate is the number of selected completed calls divided by the elapsed time since the anchor. Percentiles use the durations of those same calls. Completion-based selection retains a newly completed long tick instead of discarding it by its old start time.

The named duration scope is UDomGameEngine::Tick. It includes the game's pre/post-tick delegates and the nested UGameEngine::Tick and world tick. It excludes work outside the call and asynchronous work that outlives its return. It is elapsed wall time, not thread CPU time.

## Binary evidence

The retained server has ELF build ID `3b4ce30aed886594`. It is an x86-64 ET_EXEC image. Its lowest LOAD address is `0x200000`, not its load bias. Compact symbol records use addresses relative to that lowest address.

The exported UGameEngine vtable starts at `0x1b0a518`. Its address point starts 16 bytes later. Slot 98, at `0x1b0a838`, points directly to UGameEngine::Tick at `0x810a480`. FEngineLoop loads GEngine from `0xdfb9af0` and calls that slot at `0x8c52985`. The float argument uses xmm0 and the boolean uses esi. UGameEngine::Tick calls UWorld::Tick at `0x810a6cb` and normally returns at `0x810aa4c`.

`inspect-tick-build.py` checks the build identity, slot pointer, entry bytes, virtual dispatch, nested world call, and return without loading or writing the game. It passed against the retained installed binary on September 16, 2026. This establishes a candidate boundary, not proof of a running measurement.

An isolated actual-server baseline exposed the concrete override. GEngine's live vptr was `0x2252108`, the address point of exported `_ZTV14UDomGameEngine` at `0x22520f8`. Slot 98 at `0x2252418` points to `0x9d2eab0`. That override calls UGameEngine::Tick at `0x9d2eb6d`, between its pre/post delegates, and returns normally at `0x9d2ec2c` or through a tail call. The wrapper must target this game override. The verification script checks these bytes too.

The baseline used the official image, read-only installed game files, a separate writable Saved directory, no network, one CPU, a 2304 MiB memory limit, and a 2816 MiB combined memory/swap limit. The actual world loaded and the engine frame counter advanced. Docker reported about 883 MiB memory usage. GDB inspected the live instance and vtable without changing game files. Offline EOS errors were expected. This proves local empty-world execution, not an externally joinable session.

## Installation and verification

The library installs synchronously before spawning its initialization thread. Unknown builds and mismatched pointers must remain unmodified. The wrapper forwards the original arguments exactly once, without holding the sample lock. Hook state and code remain valid until process exit.

Completion requires a bounded actual-game run with the original world and kind left stopped. Acceptance includes authenticated values, empty-server behavior, warmup, stale state, restart reset, and C2 chart/export verification. Synthetic arithmetic tests do not replace actual-game evidence.

The implementation is split between the API producer, the C2 collector, and UI/packaging. The first two have separate owners and disjoint files. The existing telemetry store and REST server remain the only transport and history mechanisms.
