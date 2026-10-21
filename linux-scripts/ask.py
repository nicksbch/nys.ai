#!/usr/bin/env python3
"""
Send one prompt to llama-server and stream the answer to the terminal.
Standard library only.

Usage:
  # on the server (it listens on 127.0.0.1 only; SSH tunnels are disabled there)
  python3 ask.py "Explain RAID 5 in two sentences"
  python3 ask.py -f "What is the capital of France?"   # fast: skip thinking
  python3 ask.py "Summarise this: $(cat notes.txt)"
  cat question.txt | python3 ask.py          # prompt from stdin when no args

Env vars:
  LLAMA_URL      default http://127.0.0.1:8081
  LLAMA_API_KEY  only if the server was started with API_KEY
"""
import json
import os
import sys
import time
import urllib.request

URL = os.environ.get("LLAMA_URL", "http://127.0.0.1:8081").rstrip("/")
API_KEY = os.environ.get("LLAMA_API_KEY", "")

DIM, RESET = ("\033[2m", "\033[0m") if sys.stdout.isatty() else ("", "")


def main() -> None:
    args = sys.argv[1:]
    fast = bool(args) and args[0] in ("-f", "--fast")
    if fast:
        args = args[1:]
    prompt = " ".join(args)
    if not prompt and not sys.stdin.isatty():
        prompt = sys.stdin.read().strip()
    if not prompt:
        sys.exit(__doc__)

    body = json.dumps({
        "messages": [{"role": "user", "content": prompt}],
        "stream": True,
        "timings_per_token": True,
        # Qwen's and Gemma's chat templates read this; False makes it answer directly.
        "chat_template_kwargs": {"enable_thinking": not fast},
    }).encode()
    headers = {"Content-Type": "application/json"}
    if API_KEY:
        headers["Authorization"] = f"Bearer {API_KEY}"
    req = urllib.request.Request(f"{URL}/v1/chat/completions", body, headers)

    start = time.monotonic()
    first_token = None
    thinking = False
    timings = {}

    with urllib.request.urlopen(req) as resp:
        for line in resp:
            line = line.decode().strip()
            if not line.startswith("data: ") or line == "data: [DONE]":
                continue
            chunk = json.loads(line[6:])
            timings = chunk.get("timings", timings)
            if not chunk.get("choices"):
                continue
            delta = chunk["choices"][0].get("delta", {})

            # The model thinks before answering; llama-server sends that separately.
            reasoning = delta.get("reasoning_content")
            content = delta.get("content")
            if (reasoning or content) and first_token is None:
                first_token = time.monotonic()
            if reasoning:
                if not thinking:
                    print(f"{DIM}[thinking]", flush=True)
                    thinking = True
                print(reasoning, end="", flush=True)
            if content:
                if thinking:
                    print(f"\n[/thinking]{RESET}\n", flush=True)
                    thinking = False
                print(content, end="", flush=True)

    print(RESET)
    if timings:
        print(f"{DIM}--- prompt: {timings.get('prompt_n', 0)} tok @ "
              f"{timings.get('prompt_per_second', 0):.1f} tok/s | "
              f"answer: {timings.get('predicted_n', 0)} tok @ "
              f"{timings.get('predicted_per_second', 0):.1f} tok/s | "
              f"first token after {(first_token or time.monotonic()) - start:.1f}s | "
              f"total {time.monotonic() - start:.1f}s{RESET}")


if __name__ == "__main__":
    main()
