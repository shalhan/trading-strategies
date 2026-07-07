# Deploying the paper trader

The trader is one static Go binary + a state directory. It needs: outbound
HTTPS to data-api.binance.vision, a clock, and ~50MB RAM. Any always-on Linux
box works (VPS, Oracle free VM, Raspberry Pi).

## Option A — Oracle Cloud "Always Free" VM (free forever)

1. Sign up at oracle.com/cloud/free (card required for identity, not billed).
   Pick a NON-US home region with capacity — Singapore or Frankfurt.
2. Create an Always Free instance: VM.Standard.A1.Flex (Ampere ARM), 1 OCPU /
   6GB is far more than enough. Ubuntu 24.04 image. Add your SSH key.
3. On your Mac, from the repo:
       GOOS=linux GOARCH=arm64 go build -o paper-linux ./cmd/paper
       scp paper-linux ubuntu@<VM_IP>:/tmp/
       scp -r paper-state/universe.json ubuntu@<VM_IP>:/tmp/   # keep the same frozen universe
4. On the VM:
       sudo useradd -r -m -d /opt/paper paper
       sudo mkdir -p /opt/paper/state /opt/paper/data
       sudo mv /tmp/paper-linux /opt/paper/paper
       sudo mv /tmp/universe.json /opt/paper/state/
       sudo chown -R paper:paper /opt/paper
       # install the unit file (deploy/paper.service), set DISCORD_WEBHOOK, then:
       sudo systemctl daemon-reload && sudo systemctl enable --now paper
5. Check: `journalctl -u paper -f` and `curl localhost:8899/status`.
   To reach /status from your phone, open port 8899 in the VCN security list
   (or keep it closed and use: ssh -L 8899:localhost:8899 ubuntu@VM).

## Option B — Docker anywhere
    docker build -f deploy/Dockerfile -t paper .
    docker run -d --name paper --restart unless-stopped \
      -v paper-state:/state -p 8899:8899 \
      -e DISCORD_WEBHOOK=... paper

## Notifications
- Discord: channel settings → Integrations → Webhooks → copy URL → env
  DISCORD_WEBHOOK (easiest, free).
- Telegram: create a bot with @BotFather → TELEGRAM_TOKEN; message the bot,
  get your chat id (api.telegram.org/bot<TOKEN>/getUpdates) → TELEGRAM_CHAT.

## Notes
- data-api.binance.vision is Binance's public market-data CDN; if a region
  cannot reach it, pick another region (Singapore/Frankfurt are safe).
- GCP's always-free e2-micro is US-only; the data mirror usually works from
  US, but test `curl https://data-api.binance.vision/api/v3/time` first.
- Restarts are safe: structure is rebuilt from history; open PAPER positions
  are dropped (logged) and the account resumes flat.
