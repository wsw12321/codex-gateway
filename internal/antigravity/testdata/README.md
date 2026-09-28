`agy-1.2.4-native.json` derives from the local AGY 1.2.4 HTTP captures taken
on 2026-09-27. It preserves the captured main/title generation defaults,
model alias switch, complete `manage_task` and `write_to_file` schemas,
function call IDs, and `role=model` tool results. The unsigned tool loop
was observed in a separate probe. The signed variant tests handling of an
external session's signature; the adapter never creates one.

All headers were discarded. System instructions, user text, timestamps,
tool output, and the external signature were replaced with fixed test data.
Unrelated tool declarations were removed. These fixtures verify the local
wire protocol; they are not evidence of a successful Google upstream run.

The captured preview/customtools paths are historical client aliases and are
now rejected. Payload replay tests explicitly substitute the supported exact
AGY model ID; the stored captures retain their original paths.
