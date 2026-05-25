# Interactive PTY sessions

Use this when you need a **long-lived Bash** driven over **WebSocket**: PTY mode behaves like a real terminal (colors, `stty`, resize); **pipe mode** (`pty=0`) splits stdout/stderr without a TTY. **Unix/macOS/Linux only** — not supported on Windows.

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

3. **Traffic** — after a JSON `connected` frame, the server sends **binary** chunks: first byte is the channel (`0x01` stdout, `0x02` stderr in pipe mode only, `0x03` replay with an 8-byte offset header). Send **stdin** as binary: `0x00` + raw bytes. For **resize** / **signals** / **ping**, send **JSON text** frames, e.g. `{"type":"resize","cols":120,"rows":40}`, `{"type":"signal","signal":"SIGINT"}`, `{"type":"ping"}`.

4. **One WebSocket per session** — a second connection gets **409** until the first closes.

5. **End** — when Bash exits, you get a JSON `exit` frame with `exit_code` and the socket closes. Use **`DELETE /pty/:id`** to tear down the session from the server side.

## Modes

- **PTY (default)** — ANSI and TTY-aware tools work as usual.
- **Pipe** — `?pty=0`; stderr is separate binary frames. Good when you do not need a TTY.

## Notes

- Output is also buffered for **replay**; reconnect with `since=` to catch up.
- In PTY streams, **shell echo** may appear before your command’s real output, so avoid matching only on text that also appears in the typed line.
