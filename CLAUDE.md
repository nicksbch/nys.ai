This repo is an AI-first data warehouse. Everything must be automated: AI agents must be able to operate the platform, fix issues, build new features, design and evolve the data model, query it, and answer questions about the data.
Every service must expose an agent-usable interface (MCP tools or a CLI); no task may depend on a human clicking through a UI.

Main language is golang.
no scope creep without approval. the goal is to build small applications that do very few things really well over complex applications.
deploy scripts are for the remote server over ssh
