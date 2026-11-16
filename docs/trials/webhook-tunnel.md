# Starting the webhook tunnel

GitHub needs a public address to deliver webhooks to the local server. This is step 0, item 1 of [`ticket-10-plan.md`](../tickets/ticket-10-plan.md).

## 1. Start the tunnel

Run this in its own terminal and leave it open:

```
cloudflared tunnel --url http://127.0.0.1:8080
```

It prints an address like `https://<random>.trycloudflare.com`.

If you close the terminal, the tunnel stops. The address changes each time the tunnel starts, so repeat step 2 after every restart.

## 2. Point GitHub at the tunnel

1. Open the App's settings at **Settings → Developer settings → GitHub Apps → your App**.
2. Set **Webhook URL** to `https://<random>.trycloudflare.com/webhooks/github`. Keep the `/webhooks/github` path.
3. Save the change.

## 3. Check that it works

1. Start Docker and run `make dev`, because the server needs Temporal.
2. In another terminal, load the secrets file the way you do for the worker, then start the server:
   ```
   go run ./cmd/server -github-config deploy/local/github.yaml -repos deploy/local/repos.yaml
   ```
3. In the App's settings, open **Advanced → Recent Deliveries**, pick the latest delivery and click **Redeliver**. If no `ping` is listed, any recent delivery will do.
4. Check the response. `202` means the whole path works.

## If it fails

| Response | Likely cause |
|---|---|
| `502` or a similar error from Cloudflare | The server isn't running, or it isn't on port 8080. |
| `404` | The webhook URL is missing `/webhooks/github`. |
| `401` | The request reached the server, but the signature check failed. The webhook secret probably doesn't match the App's. |
| `503` | The request reached the server, but it couldn't take the delivery, for example because its queue is full. |
