# chatapi

A Go package, not a service: analyst and nexus talk to their model with it.
It speaks the chat completions API (llama-server, or any API that speaks
it), reads a config directory's `models.json` and `prompt.md`, and keeps a
conversation in the API's message format, so it can be stored as is and
sent again for a follow-up.

```go
import "chatapi" // go.mod: replace chatapi => ../chatapi

cfg, err := chatapi.LoadConfig("/etc/nexus", os.Getenv, "/etc/nexus.env")
r, msg, err := chatapi.Complete(ctx, cfg.Models[cfg.Default], cfg.Prompt, msgs, tools)
```

`Ping` checks that a model's API answers; `TestTools` checks that it also
calls a tool. foreman has its own model code, since it also speaks
Anthropic's Messages API.

Its tests run in `../deploy-all.sh`'s preflight whenever analyst or nexus
is deployed.
