# garage-ui

A small web UI for the Garage S3 store set up by `../linux-scripts/garage-setup.sh`.
It is a single Go binary with the page built in.

- **Browse:** buckets in a sidebar, folders, a clickable path, a filter box, and
  "Load more" for very large folders. Each folder has its own URL, so you can
  bookmark it.
- **Upload:** pick files or a whole folder, or drag and drop (folders included).
  Three files upload at a time, with progress bars. Files stream through to
  Garage without being saved to disk.
- **Download or view:** click a file name to open it in the browser, or use ⤓
  to download. Videos can be skipped through.
- **Rename (F2), Copy to…, Move to…, Delete (Del key), New folder:**
  - Copy and move work across buckets. Folders are handled with everything
    inside them.
  - It asks before overwriting anything that already exists, and refuses to
    move a folder into itself.
  - A move only deletes the originals after every copy has succeeded.
  - Files over 5 GB are copied in parts, because S3 can't copy them in one
    request.

The bucket list only shows buckets that the key can access. Creating or deleting
buckets isn't supported.

## Files

| File         | What it is                                                                      |
|--------------|---------------------------------------------------------------------------------|
| `main.go`    | The server: an S3 client and a JSON API for the page                            |
| `index.html` | The page (plain HTML and JavaScript, built into the binary)                     |
| `deploy.sh`  | Run on your Mac: builds the binary and installs it on the server                |
| `install.sh` | Run on the server by `deploy.sh`: installs the binary, the key and the service  |

## Run it locally

```sh
go build -o garage-ui . && ./garage-ui     # then open http://127.0.0.1:8080
```

Credentials come from the usual AWS environment variables or your `~/.aws`
profile:

```sh
AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...
AWS_DEFAULT_REGION=garage                    # default: garage
AWS_ENDPOINT_URL=http://192.168.1.169:3900   # this is the default
```

Flags:

| Flag            | Default                                          | Meaning                                  |
|-----------------|--------------------------------------------------|------------------------------------------|
| `-listen`       | `127.0.0.1:8080`                                 | Address to serve the UI on               |
| `-endpoint-url` | `$AWS_ENDPOINT_URL`, else `http://192.168.1.169:3900` | S3 endpoint                         |
| `-region`       | from env or profile, else `garage`               | S3 region                                |
| `-profile`      |                                                  | Profile name from `~/.aws`               |
| `-auth`         | `$GARAGE_UI_AUTH`                                | Require a login, given as `user:pass`    |

## Deploy to the server

```sh
./deploy.sh
```

You need SSH access to the server and `sudo` there (you may be asked for your
password). Your `~/.ssh/config` supplies the SSH user.

Settings are passed as environment variables:

```sh
HOST=nicolas@192.168.1.169 SSH_PORT=2222 LISTEN='[::]:8080' BUCKETS="home photos" ./deploy.sh
```

| Variable   | Default                 | Meaning                                                          |
|------------|-------------------------|------------------------------------------------------------------|
| `HOST`     | `192.168.1.169`         | SSH destination                                                  |
| `SSH_PORT` | `2222`                  | SSH port on the server                                           |
| `LISTEN`   | `[::]:8080`             | Where the UI listens. Use `127.0.0.1:8080` to keep it local to the server |
| `BUCKETS`  | `home`                  | Buckets the UI's key may read and write (separated by spaces)    |
| `KEY_NAME` | `garage-ui-key`         | Name of the Garage key the UI uses                               |
| `UI_USER`  | `admin`                 | User name for the login                                          |
| `ENDPOINT` | `http://127.0.0.1:3900` | S3 endpoint, as seen from the server                             |

Run it again whenever you want to ship a new version. The key and the login stay
the same.

### What `deploy.sh` does (on your Mac)

1. Connects over SSH and detects the server's CPU type (x86_64 or arm64).
2. Builds a static Linux binary.
3. Copies the binary and `install.sh` to a temporary folder on the server.
4. Runs `install.sh` there with `sudo`, then deletes the temporary folder.

### What `install.sh` does (on the server)

It follows the same layout as `garage-setup.sh`. You can also run it by hand with
`sudo bash install.sh ./garage-ui`.

1. Stops early if Garage isn't installed and running.
2. Installs the binary to `/usr/local/bin/garage-ui`.
3. Creates its own Garage key, `garage-ui-key`, and gives it read and write
   access to each bucket in `BUCKETS`. The UI never uses your personal key, so
   you can revoke its access on its own.
4. On the first run only, writes `/etc/garage-ui.env`, readable by root only. It
   holds that key, the S3 endpoint and a randomly generated login
   (`admin:<password>`). Later runs leave this file alone.
5. Writes `/etc/systemd/system/garage-ui.service`, with the same hardening as
   `garage.service` (`ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`,
   `NoNewPrivileges`). It runs under a throwaway system user
   (`DynamicUser=yes`), so no service account is needed. Then it enables and
   restarts the service.
6. Waits until the UI responds. If it doesn't, it shows the last log lines.
7. Prints the URL and the login. The password is only shown on the first run.

## After deploying

- **URL:** `http://192.168.1.169:8080`
- **Login:** `sudo grep GARAGE_UI_AUTH /etc/garage-ui.env`
- **Logs:** `journalctl -u garage-ui -f`
- **Restart:** `sudo systemctl restart garage-ui`
- **Firewall:** if `ufw` is active, run `sudo ufw allow 8080/tcp` (the script
  reminds you).
- **New buckets** aren't added automatically. Re-run with the full list:
  `BUCKETS="home newbucket" ./deploy.sh`
- **Change the login or key:** edit `/etc/garage-ui.env`, then restart the
  service. To start fresh, delete the file and run `./deploy.sh` again.

## Security

Anyone who can log in has full read and write access to the key's buckets.

- **Login:** locally the UI listens on localhost only. On the server it listens
  on the LAN, so a login is always set.
- **Forged requests are blocked:** every change needs an `X-Garage-UI` header
  that only the app's own page sends. Other websites can't add it without a CORS
  preflight, which the server never grants.
- **Opened files run sandboxed:** files are served with
  `Content-Security-Policy: sandbox` and `nosniff`, so an HTML file stored in a
  bucket can't act as the app.
- **Use on the LAN only:** the UI speaks plain HTTP. Don't expose it to the
  internet without a TLS proxy in front.

## Uninstall

```sh
sudo systemctl disable --now garage-ui
sudo rm /usr/local/bin/garage-ui /etc/systemd/system/garage-ui.service /etc/garage-ui.env
sudo systemctl daemon-reload
sudo garage key delete --yes garage-ui-key
```
