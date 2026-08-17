/**
 * The node's own setup page, served on this machine's dsh web server.
 *
 * It exists because of a timing problem nothing else solves. dsh is often
 * already running when the control plane appears — a plugin added to a live
 * process, or a fleet stood up afterwards — and at that moment there is no way
 * to point the machine at it. dsh has no CLI for plugin configuration (`dsh
 * plugin` forwards to pnpm), and its Settings → Plugins page renders a
 * hardcoded set of cards gated by an allowlist in the harness, so a plugin
 * shipped from outside cannot appear there. That leaves editing a YAML file by
 * hand, on that machine, which is not something to ask of whoever is sitting in
 * front of a browser that is already open.
 *
 * Writing the config through the loader entry is what makes it take effect:
 * Cordis persists the change and reloads this plugin, so the uplink comes up
 * without restarting dsh.
 *
 * @module @dsh-fleet/node/setup
 */

import type { Context } from '@deepseek-ai/cordis'
import type {} from '@deepseek-ai/cordis-plugin-loader'
import type {} from '@deepseek-ai/dsh-host-webserver'
import type { IncomingMessage, ServerResponse } from 'node:http'

/**
 * Where the page lives on this machine's own server.
 *
 * Deliberately not under `/_fleet`: that prefix belongs to the control plane on
 * its own origin, and two things answering to one name across a proxy is a
 * confusion waiting to be debugged.
 */
export const SETUP_PATH = '/_dshf-setup'

/** What the page shows and what a save may change. */
export interface SetupFields {
  url: string
  nodeId: string
  username: string
  token: string
  label: string
}

/** How the page reports the uplink without reaching into it. */
export interface SetupStatus {
  /** `connected`, `connecting`, or `offline`. */
  state: string
  detail: string
}

/** Everything the routes need from the plugin around them. */
export interface SetupOptions {
  /** Current effective values, with the token already masked. */
  read: () => { fields: SetupFields; tokenSet: boolean; status: SetupStatus }
  /** Persist a change through the loader, which reloads this plugin. */
  save: (fields: SetupFields) => Promise<void>
}

/**
 * Mount the setup page, returning a disposer.
 *
 * Registered on this machine's own server rather than proxied, and the uplink
 * refuses to forward this prefix — see the note there. Configuring a machine is
 * a local act, and a page that could re-point a node at a different control
 * plane must not be reachable from whoever happens to be signed in to the
 * current one.
 *
 * @param ctx - the plugin context; `webServer` is read, not injected, so a
 *   composition without one simply gets no page.
 * @param options - the read and save hooks.
 * @returns a disposer that unregisters both routes, or undefined when there is
 *   no server to register on.
 */
export function mountSetup(ctx: Context, options: SetupOptions): (() => void) | undefined {
  const server = ctx.get('webServer')
  if (server === undefined) return undefined

  const disposers = [
    server.register({
      kind: 'exact',
      path: SETUP_PATH,
      handler: (req: IncomingMessage, res: ServerResponse) => {
        const url = new URL(req.url ?? SETUP_PATH, 'http://localhost')
        send(res, 200, page(options.read(), url.searchParams.get('saved') !== null, url.searchParams.get('error')))
      },
    }),
    server.register({
      kind: 'exact',
      path: `${SETUP_PATH}/save`,
      handler: async (req: IncomingMessage, res: ServerResponse) => {
        if (req.method !== 'POST') {
          send(res, 405, page(options.read(), false, 'Use the form to save.'))
          return
        }
        try {
          const form = await readForm(req)
          await options.save({
            url: form.get('url') ?? '',
            nodeId: form.get('nodeId') ?? '',
            username: form.get('username') ?? '',
            // An empty token field means "keep the one already stored", so a
            // masked value is never saved back over a real secret.
            token: form.get('token') ?? '',
            label: form.get('label') ?? '',
          })
          // Redirect so a refresh cannot re-submit the form.
          res.statusCode = 303
          res.setHeader('location', `${SETUP_PATH}?saved`)
          res.end()
        } catch (error: unknown) {
          send(res, 400, page(options.read(), false, String(error instanceof Error ? error.message : error)))
        }
      },
    }),
  ]
  return () => { for (const dispose of disposers) dispose() }
}

function send(res: ServerResponse, status: number, html: string): void {
  res.statusCode = status
  res.setHeader('content-type', 'text/html; charset=utf-8')
  // The page shows configuration; a cached copy in a shared browser is not
  // wanted, and a stale one would misreport the connection.
  res.setHeader('cache-control', 'no-store')
  res.end(html)
}

/** Read a urlencoded form body, bounded so a bad request cannot exhaust memory. */
async function readForm(req: IncomingMessage): Promise<Map<string, string>> {
  const chunks: Buffer[] = []
  let size = 0
  for await (const chunk of req) {
    const buf = chunk as Buffer
    size += buf.length
    if (size > 64 * 1024) throw new Error('the form is too large')
    chunks.push(buf)
  }
  const params = new URLSearchParams(Buffer.concat(chunks).toString('utf8'))
  const form = new Map<string, string>()
  for (const [key, value] of params) form.set(key, value.trim())
  return form
}

const escape = (value: string): string =>
  value.replace(/[&<>"']/g, c => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c] ?? c))

/**
 * The page.
 *
 * Hand-written HTML rather than a dsh settings card, because appearing in that
 * dialog would mean adopting the harness's client bundle format, its Typert
 * codegen, and an allowlist that a third-party plugin cannot enter — three
 * internal contracts for one form.
 */
function page(
  current: { fields: SetupFields; tokenSet: boolean; status: SetupStatus },
  saved: boolean,
  failure: string | null,
): string {
  const { fields, tokenSet, status } = current
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="color-scheme" content="light dark">
<title>Fleet setup · dsh</title>
<style>
:root {
  --bg:#f4f6f9; --surface:#fff; --sunk:#eef2f7; --ink:#16233a; --muted:#64758f;
  --rule:#dde3ec; --accent:#1f5fa8; --on-accent:#fff;
  --ok:#2e7259; --warn:#a96f1c; --danger:#a4322c; --shadow:rgba(16,32,56,.10);
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg:#0d141e; --surface:#182231; --sunk:#121b27; --ink:#e3eaf4; --muted:#93a3b9;
    --rule:#2c3b52; --accent:#7db0ea; --on-accent:#0d141e;
    --ok:#66bb96; --warn:#d5a55e; --danger:#e08b84; --shadow:rgba(0,0,0,.40);
  }
}
* { box-sizing:border-box }
body {
  margin:0; min-height:100dvh; background:var(--bg); color:var(--ink);
  font:16px/1.55 system-ui,-apple-system,"Segoe UI","PingFang SC",sans-serif;
  padding:calc(env(safe-area-inset-top) + 1.5rem) 1rem calc(env(safe-area-inset-bottom) + 2rem);
}
main { max-width:34rem; margin:0 auto }
.brand { display:flex; align-items:center; gap:.5rem; font-weight:650; margin-bottom:1.4rem }
.mark { width:1.4rem; height:1.4rem; border-radius:6px; background:var(--accent); color:var(--on-accent);
        display:grid; place-items:center; font-size:.7rem; font-weight:700 }
h1 { font-size:1.4rem; letter-spacing:-.015em; margin:0 0 .2rem }
.lede { color:var(--muted); font-size:.88rem; margin:0 0 1.2rem }
.card { background:var(--surface); border:1px solid var(--rule); border-radius:12px;
        padding:1.1rem 1.2rem; margin-bottom:1rem }
.state { display:flex; align-items:center; gap:.5rem; font-size:.9rem }
.dot { width:.55rem; height:.55rem; border-radius:50%; background:var(--muted); flex:none }
.dot.connected { background:var(--ok) } .dot.connecting { background:var(--warn) }
.state .detail { color:var(--muted); font-size:.82rem; margin-left:auto }
label { display:flex; flex-direction:column; gap:.3rem; font-size:.78rem; color:var(--muted); margin-bottom:.8rem }
input { font:inherit; padding:.55rem .65rem; border-radius:8px; border:1px solid var(--rule);
        background:var(--sunk); color:var(--ink) }
input:focus-visible { outline:2px solid var(--accent); outline-offset:1px }
.help { font-size:.74rem; color:var(--muted) }
button { font:inherit; font-weight:600; padding:.55rem 1rem; border:0; border-radius:8px;
         background:var(--accent); color:var(--on-accent); cursor:pointer }
.banner { border-radius:8px; padding:.55rem .75rem; font-size:.85rem; margin-bottom:1rem }
.banner.ok { background:color-mix(in srgb,var(--ok) 12%,transparent); border:1px solid var(--ok); color:var(--ok) }
.banner.bad { background:color-mix(in srgb,var(--danger) 12%,transparent); border:1px solid var(--danger); color:var(--danger) }
code { font-family:ui-monospace,"Cascadia Mono",Consolas,monospace; font-size:.85em }
</style>
</head>
<body>
<main>
  <p class="brand"><span class="mark" aria-hidden="true">dF</span> dsh-fleet</p>
  <h1>Connect this machine</h1>
  <p class="lede">Point this dsh at a control plane so you can reach it from a phone or another computer.</p>

  ${saved ? '<p class="banner ok">Saved. The plugin reloaded; watch the status above.</p>' : ''}
  ${failure === null ? '' : `<p class="banner bad">${escape(failure)}</p>`}

  <div class="card">
    <p class="state">
      <span class="dot ${escape(status.state)}" aria-hidden="true"></span>
      <strong>${escape(status.state === 'connected' ? 'Connected' : status.state === 'connecting' ? 'Connecting' : 'Not connected')}</strong>
      <span class="detail">${escape(status.detail)}</span>
    </p>
  </div>

  <form class="card" method="post" action="${SETUP_PATH}/save">
    <label>Control plane address
      <input name="url" value="${escape(fields.url)}" placeholder="wss://fleet.example.com/uplink" autocapitalize="none" spellcheck="false">
      <span class="help">The console shows this beside a new token. Empty stays offline.</span>
    </label>
    <label>Your console account
      <input name="username" value="${escape(fields.username)}" placeholder="admin" autocapitalize="none" spellcheck="false">
      <span class="help">With it, this machine registers itself. Leave empty if the token below is a machine token.</span>
    </label>
    <label>Token
      <input name="token" type="password" placeholder="${tokenSet ? 'unchanged' : 'ut_…'}" autocomplete="off">
      <span class="help">${tokenSet ? 'A token is stored. Leave this empty to keep it.' : 'From “Your account” in the console.'}</span>
    </label>
    <label>This machine's name
      <input name="nodeId" value="${escape(fields.nodeId)}" placeholder="${escape(fields.nodeId)}" autocapitalize="none" spellcheck="false">
      <span class="help">Defaults to this machine's hostname.</span>
    </label>
    <label>Display name <span class="help">Optional.</span>
      <input name="label" value="${escape(fields.label)}" placeholder="e.g. Work laptop">
    </label>
    <button type="submit">Save and connect</button>
  </form>

  <p class="lede">This page is only served to this machine — it is not forwarded through the
    control plane, because it can change which control plane this machine answers to.</p>
</main>
</body>
</html>`
}
