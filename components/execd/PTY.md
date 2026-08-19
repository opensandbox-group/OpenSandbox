# Interactive PTY sessions

Use this when you need a **long-lived shell** driven over **WebSocket**: PTY mode behaves like a real terminal (colors, `stty`, resize); **pipe mode** (`pty=0`) splits stdout/stderr without a TTY. execd uses Bash when available and falls back to `sh` on minimal images without Bash. **Unix/macOS/Linux only** — not supported on Windows.

## Typical usage

1. **Create a session** (shell starts on the first WebSocket, not here):

   ```bash
   curl -s -X POST http://127.0.0.1:44772/pty \
     -H 'Content-Type: application/json' \
     -d '{"cwd":"/tmp"}'
   # → { "session_id": "<id>" }
   ```

2. **Open WebSocket** — default is PTY mode:

   ```
   ws://127.0.0.1:44772/pty/<session_id>/ws
   ```

   | Query | Use |
   |-------|-----|
   | `pty=0` | Pipe mode instead of PTY |
   | `since=<offset>` | After reconnect, replay from byte offset (use `output_offset` from `GET /pty/:id`) |
   | `takeover=1` | Evict the current holder instead of getting **409**, then attach to the same shell (combine with `since=` to replay scrollback) |
   | `mode=viewer` | Attach as a concurrent read-only viewer after the shell is running; combine with `since=` for scrollback |

3. **Traffic** — the JSON `connected` frame identifies the connection with `mode` (`pty` or `pipe`) and `role` (`holder` or `viewer`). A holder receives **binary** chunks with first byte `0x01` (stdout) or `0x02` (stderr in pipe mode only), and sends **stdin** as `0x00` + raw bytes. A viewer receives `0x03` replay frames for both retained and live output: an 8-byte big-endian offset followed by raw bytes. For **resize** / **signals** / **ping**, send **JSON text** frames, e.g. `{"type":"resize","cols":120,"rows":40}`, `{"type":"signal","signal":"SIGINT"}`, `{"type":"ping"}`.

4. **One read/write holder, multiple viewers** — a second read/write connection gets **409** until the first closes, unless it passes **`?takeover=1`**: the current holder is then closed with WebSocket code **4001** (reason `TAKEN_OVER`) and the new connection takes over the **same** shell. Any number of `?mode=viewer` connections can watch replay and live output without acquiring or evicting that holder.

5. **End** — when the shell exits, you get a JSON `exit` frame with `exit_code` and the socket closes. Use **`DELETE /pty/:id`** to tear down the session from the server side.

## Modes

- **PTY (default)** — ANSI and TTY-aware tools work as usual.
- **Pipe** — `?pty=0`; stderr is separate binary frames. Good when you do not need a TTY.
- **Viewer** — `?mode=viewer`; requires a running session and rejects binary stdin plus JSON `stdin`, `signal`, and `resize` frames with `READ_ONLY`. `ping` remains available. The server closes a viewer after five rejected mutating frames.

## Notes

- Commands running under the `sh` fallback must use syntax supported by the image's `sh` implementation.
- Output is also buffered for **replay**; reconnect with `since=` to catch up.
- Viewer output is delivered from the bounded replay stream, so WebSocket backpressure from a slow viewer cannot block the read/write holder's live output pipe. In pipe mode this is a combined stream without separate stdout/stderr channels.
- A viewer can attach only while the shell is running; when it exits, viewers receive the `exit` frame and close. If a holder later starts the same session again, its bounded replay buffer is retained, so a viewer reconnecting with `since=0` can receive retained output from the preceding shell lifetime.
- In PTY streams, **shell echo** may appear before your command’s real output, so avoid matching only on text that also appears in the typed line.
