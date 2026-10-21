# home

A small homepage for the server's web UIs: one card per service, with a dot
showing whether it answers right now. Click a card to open the service. It is a
single Go binary with the page built in.

- **Links follow your address:** each link uses the host you opened the page
  on, plus the service's port. `http://192.168.1.169:8000` links to
  `http://192.168.1.169:8090`, and a hostname works the same way.
- **Status is checked on the server:** every 10 seconds (while the tab is
  visible) the page asks the server, which tries each service on `127.0.0.1`.
  Any HTTP answer counts as up, including a login prompt (401) or a redirect.
  No answer within 2 seconds, or a 5xx error, counts as down.

Default services:

| Name            | Port | Path  |
|-----------------|------|-------|
| `vigil`         | 8090 | `/`   |
| `relay`         | 8091 | `/ui` |
| `garage-ui`     | 8080 | `/`   |
| `lake-ui`       | 8092 | `/`   |
| `url-to-bucket` | 8093 | `/`   |
| `foreman`       | 8096 | `/ui` |

## Files

| File         | What it is                                                         |
|--------------|--------------------------------------------------------------------|
| `main.go`    | The server: serves the page and checks each service                |
| `index.html` | The page (plain HTML and JavaScript, built into the binary)        |
| `deploy.sh`  | Run on your Mac: builds the binary and installs it on the server   |
| `install.sh` | Run on the server by `deploy.sh`: installs the binary and service  |

## Run it locally

```sh
go build -o home . && ./home     # then open http://127.0.0.1:8000
```

| Flag        | Default                                                | Meaning                                       |
|-------------|--------------------------------------------------------|-----------------------------------------------|
| `-listen`   | `127.0.0.1:8000`                                       | Address to serve the page on                  |
| `-services` | `vigil=8090 relay=8091/ui garage-ui=8080 lake-ui=8092 url-to-bucket=8093 foreman=8096/ui` | Services, as `name=port[/path]`, space separated |

## Deploy to the server

```sh
./deploy.sh                       # or, with everything else: ONLY=home ../deploy-all.sh
```

| Variable   | Default                                                | Meaning                                   |
|------------|--------------------------------------------------------|-------------------------------------------|
| `HOST`     | `192.168.1.169`                                        | SSH destination                           |
| `SSH_PORT` | `2222`                                                 | SSH port on the server                    |
| `LISTEN`   | `[::]:8000`                                            | Where the page listens                    |
| `SERVICES` | `vigil=8090 relay=8091/ui garage-ui=8080 lake-ui=8092 url-to-bucket=8093 foreman=8096/ui` | Services to show                          |

The systemd unit is rewritten on every run, so keep non-default values in
`../deploy.env` (`HOME_LISTEN`, `HOME_SERVICES`).

## After deploying

- **URL:** `http://192.168.1.169:8000`
- **Logs:** `journalctl -u home -f`
- **Firewall:** if `ufw` is active, run `sudo ufw allow 8000/tcp` (the script
  reminds you).

## Security

The page only shows links and up/down states, so it has no login; each service
still asks for its own. It speaks plain HTTP: keep it on the LAN.

## Uninstall

```sh
sudo systemctl disable --now home
sudo rm /usr/local/bin/home /etc/systemd/system/home.service
sudo systemctl daemon-reload
```
