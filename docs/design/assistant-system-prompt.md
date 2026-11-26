# Assistant system prompt

The prompt lives in [`internal/assistant/system_prompt.md`](../../internal/assistant/system_prompt.md) (embedded in the binary). The route map is filled in from `assistant.Routes`; `TestRoutesExist` fails when a route is missing from the web router.
