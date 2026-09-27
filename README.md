# Blink

Click a button and get a real Google Cloud VM, with a shell into it right in your browser. Every VM deletes itself when its timer runs out.

## Setup (once)

```sh
brew install --cask gcloud-cli
gcloud auth login --update-adc
gcloud config set project YOUR_PROJECT_ID   # gcloud projects list shows yours
```

The project needs billing turned on and the Compute Engine API enabled (`gcloud services enable compute.googleapis.com`). If anything is missing, the page names the next command to run. After running it, click **Check again**; you don't need to restart Blink.

## Run

```sh
go install github.com/Tmwakalasya/blink@latest
blink
```

Or run `go run .` from a clone. Either way, this opens http://localhost:8080. Click **Start VM**.

## What happens when you click

1. **Request**: Blink asks Compute Engine for an e2-micro running Debian 12, with a fresh SSH key made for that one VM.
2. **Provision**: Google allocates the VM and gives it a public IP.
3. **Boot**: Blink waits for port 22 to answer.
4. **SSH**: Blink checks the VM's host key against the one Google published for it, then opens a shell in the page.

## Staying cheap

- Each VM is created with `maxRunDuration` and `instanceTerminationAction: DELETE`, so Google deletes it when the timer runs out, even if Blink has crashed or your laptop is closed.
- Blink runs one VM at a time, and **end** deletes it immediately.
- The default VM (an e2-micro in us-central1) falls under Google Cloud's free tier, which covers one e2-micro's worth of hours a month. Outside the free tier, a 30-minute VM costs less than a cent.

## Flags

| Flag | Default | |
|---|---|---|
| `-project` | your gcloud project | |
| `-zone` | `us-central1-a` | |
| `-machine` | `e2-micro` | |
| `-image` | Debian 12 | any image path, e.g. `projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64` |
| `-ttl` | `30m` | 1m to 24h |
| `-network` | `default` | needs a firewall rule allowing SSH; Blink warns if it can't find one |
| `-addr` | `localhost:8080` | |
| `-open` | `true` | open the page in your browser |

## From your own terminal

Each VM's key and verified host key live in `~/.blink/<vm>/`. The **copy ssh** button gives you the exact `ssh` command.

## Safety

- Blink only touches VMs labeled `blink=true`. It won't open or delete anything else in your project.
- The server only answers on localhost and rejects cross-site requests, so a web page you visit can't summon VMs or open shells.
- The SSH host key comes from the VM's guest attributes, read through the authenticated Compute API, so the first connection is verified rather than trusted blindly. If an image never publishes its host key, Blink falls back to trusting the first key it sees, the way `ssh` does, and says so.

## Tests

```sh
go test -race ./...
```

The tests drive the whole summon and terminal flow against a fake Compute Engine and a real in-process SSH server, so they never touch your Google Cloud project.
