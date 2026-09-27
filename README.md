# Blink

Temporary Linux VMs on Google Cloud, with a terminal in the browser. Pick a size and how long you need it; Blink starts the VM, opens its shell on the page, and Compute Engine deletes it when the time is up.

Run it on your laptop for yourself, or on Cloud Run to share it: people sign in with Google, and Blink keeps spending under a budget you set. Medium and Large VMs open VS Code in the browser.

## On your laptop

```sh
brew install --cask gcloud-cli
gcloud auth login --update-adc
gcloud config set project YOUR_PROJECT_ID   # gcloud projects list shows yours

go install github.com/Tmwakalasya/blink@latest
blink
```

Or run `go run .` from a clone. Either way, this opens http://localhost:8080. The project needs billing turned on and the Compute Engine API enabled (`gcloud services enable compute.googleapis.com`). If anything is missing, the page names the next command to run. After running it, click **Check again**; you don't need to restart Blink.

Without sign-in, Blink only answers on localhost.

## Sharing it

Deploy Blink to Cloud Run. It gets an address like `https://blink-123456789012.us-central1.run.app`, stays within Cloud Run's free tier, and needs no domain.

1. **Create a Google sign-in client.** In the Cloud console, go to *Google Auth Platform*. Set up branding (the app name and your email), and choose *External* as the audience with *In production* status; Blink only asks for an email address, so there's no review. Then create a client of type *Web application* with these authorized JavaScript origins:
   - `https://blink-PROJECT_NUMBER.us-central1.run.app` (`gcloud projects describe YOUR_PROJECT_ID --format='value(projectNumber)'` gives the number)
   - `http://localhost:8080`, if you want to try sign-in locally with `blink -client-id ...`

2. **Deploy.**

   ```sh
   CLIENT_ID=123-abc.apps.googleusercontent.com ADMINS=you@example.com deploy/cloudrun.sh
   ```

   The script enables the APIs it needs, creates a `blink-server` service account that can only manage VMs, creates a bucket for Blink's state, and ships the code. Run it again to ship updates. `BUDGET` (default 10 dollars), `WEEKLY_HOURS` (4), `MAX_VMS` (10), `SIZES` and `MAX_TTL` (2h) change the limits.

3. **Let people in.** Sign in with an admin email and add emails under *Who can sign in*, one per line, or `@school.edu` for everyone at a domain. Anyone else who signs in can request access, and you let them in or dismiss them from the same page. Someone taken off the list is signed out on their next request.

Google Cloud bills after the fact, and its budgets only send email, so Blink enforces the budget itself. It records each VM in a ledger, counts a running VM's whole lifetime until it ends, and refuses new VMs that would go over.

## A pre-built image

```sh
deploy/build-image.sh
```

This bakes VS Code, git, pip, venv and build tools into an image in your project and trims boot-time services, so VMs are ready sooner and VS Code starts with the OS instead of installing at boot. On a Medium VM that took click-to-shell from 44.5 s to 31 s, with VS Code ready a second later. Blink uses the newest image in the `blink` family automatically; restart Blink after building, and re-run the script now and then to refresh the tools. It costs a few cents a month to store.

## Sizes

| | Machine | CPU / memory | Estimated cost |
|---|---|---|---|
| Small | e2-micro | 2 shared vCPUs / 1 GB | 1.5¢ an hour |
| Medium | e2-medium | 2 shared vCPUs / 4 GB | 4¢ an hour |
| Large | e2-standard-4 | 4 vCPUs / 16 GB | 14¢ an hour |

Estimates are for us-central1 and include the public IP and the 10 GB balanced disk (the standard disk type is too slow to boot from quickly). They ignore the free tier, so real bills come in lower.

## What happens when you start a VM

1. **Request**: Blink checks the limits, then asks Compute Engine for the VM with an SSH key made for that VM alone.
2. **Provision**: Google allocates the VM and gives it a public IP.
3. **Boot**: Blink waits for port 22 to answer.
4. **SSH**: Blink checks the VM's host key against the one Google published for it, then opens a shell on the page.

The shell lives on the server, so reloading the page, or Cloud Run cutting a connection after an hour, reattaches to it with recent output intact.

## Settings

Each flag also reads an environment variable, which is how Cloud Run sets them.

| Flag | Variable | Default | |
|---|---|---|---|
| `-project` | `BLINK_PROJECT` | your gcloud project | |
| `-zone` | `BLINK_ZONE` | `us-central1-a` | |
| `-image` | `BLINK_IMAGE` | the pre-built image, or Debian 12 | any image path, e.g. `projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64` |
| `-network` | `BLINK_NETWORK` | `default` | needs a firewall rule allowing SSH; Blink warns if it can't find one |
| `-sizes` | `BLINK_SIZES` | `small,medium,large` | |
| `-max-ttl` | `BLINK_MAX_TTL` | `2h` | lifetimes on offer are 30 min, 1 hr and 2 hr |
| `-budget` | `BLINK_BUDGET` | none | dollars, across everyone |
| `-weekly-hours` | `BLINK_WEEKLY_HOURS` | none | VM hours per person per week |
| `-max-vms` | `BLINK_MAX_VMS` | `10` | VMs running at once, across everyone |
| `-client-id` | `BLINK_CLIENT_ID` | none | Google OAuth client ID; turns on sign-in |
| `-admins` | `BLINK_ADMINS` | none | emails that manage who can sign in |
| `-state` | `BLINK_STATE` | `~/.blink` | keys, the ledger and who can sign in |
| `-addr` | `PORT` | `localhost:8080` | |

## Safety

- Blink only touches VMs labeled `blink=true`, and each person only sees and opens their own, including their VS Code.
- Each VM gets its own SSH key and refuses the project's shared keys.
- The SSH host key comes from the VM's guest attributes, read through the authenticated Compute API, so the first connection is verified rather than trusted blindly. If an image never publishes its host key, Blink falls back to trusting the first key it sees, the way `ssh` does, and says so.
- Session cookies are signed, HttpOnly and SameSite=Lax; cross-site requests are refused.

## Tests

```sh
go test -race ./...
```

The tests cover starting VMs, sign-in, the limits and the terminal, against a fake Compute Engine and a real in-process SSH server, so they never touch your Google Cloud project.
