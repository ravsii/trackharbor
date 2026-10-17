package web

const settingsTemplate = `{{define "settings"}}
<section class="content-section users-panel">
  <header class="section-heading"><h2>Users</h2></header>
  <p class="helper">All users can manage accounts. Credentials and collections stay personal.</p>
  <form class="form" id="create-user-form" action="/settings/users?account_id={{.User.ID}}" method="post" hx-post="/settings/users" hx-target="#content" hx-select="#content" hx-swap="outerHTML" hx-confirm="Create this user?" hx-disabled-elt="find button" data-feedback="users-feedback">
    <div class="grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto] items-end gap-3 max-[700px]:grid-cols-1">
      <label class="field">Username<input name="username" autocomplete="off" pattern="[A-Za-z0-9._\-]{1,64}" maxlength="64" placeholder="New username" required></label>
      <label class="field">Password<input type="password" name="password" autocomplete="new-password" placeholder="New password" required></label>
      <div><button class="button primary" type="submit">{{template "icon" "checkmark-circle-02"}}Create user</button></div>
    </div>
  </form>
  <div id="users-feedback" class="feedback" role="status" aria-live="polite"></div>
  <ul class="user-list">{{range .Users}}
    <li class="user-item" data-username="{{.Username}}">
      <div class="user-summary">
        <div class="row-name"><strong>{{.Username}}</strong>{{if eq .ID $.User.ID}} <span class="meta">(you)</span>{{end}}</div>
        <div class="actions">
          <button type="button" class="button responsive-action" data-password-toggle aria-controls="password-editor-{{.ID}}" aria-expanded="false" aria-label="Change password for {{.Username}}" title="Change password">{{template "icon" "reload"}}<span class="action-label">Change password</span></button>
          <form class="user-delete" action="/settings/users/{{.ID}}/delete?account_id={{$.User.ID}}" method="post" hx-post="/settings/users/{{.ID}}/delete" hx-target="#content" hx-select="#content" hx-swap="outerHTML" hx-confirm="Delete this user, their source credentials and playlist? Shared music files will be kept." hx-disabled-elt="find button" data-feedback="users-feedback">
            <input type="hidden" name="confirm" value="yes"><button type="submit" class="button danger responsive-action" aria-label="Delete user {{.Username}}" title="Delete user">{{template "icon" "delete-02"}}<span class="action-label">Delete user</span></button>
          </form>
        </div>
      </div>
      <form class="form password-editor user-password" id="password-editor-{{.ID}}" hidden action="/settings/users/{{.ID}}/password?account_id={{$.User.ID}}" method="post" hx-post="/settings/users/{{.ID}}/password" hx-target="#content" hx-select="#content" hx-swap="outerHTML" hx-confirm="Change this user's password and sign out all their sessions?" hx-disabled-elt="find button" data-feedback="users-feedback">
        <label class="field">New password for {{.Username}}<input type="password" name="password" autocomplete="new-password" required></label>
        <p class="helper">Changing a password signs this user out everywhere.</p>
        <div class="actions"><button type="submit" class="button primary">{{template "icon" "checkmark-circle-02"}}Save password</button></div>
      </form>
    </li>
  {{end}}</ul>
</section>

<section class="content-section">
  <header class="section-heading"><h2>Downloads</h2></header>
  <form class="form" id="download-settings-form" action="/settings/downloads?account_id={{.User.ID}}" method="post">
    <label class="field">Maximum download attempts
      <input name="download_retries" type="number" min="1" step="1" required value="{{.DownloadRetries}}"
        aria-describedby="download-retries-help download-settings-feedback">
    </label>
    <p class="helper" id="download-retries-help">Total download cycles per track, including the first attempt. Each cycle tries direct access and the configured proxies. Minimum: 1.</p>
    <p class="helper">Changes apply to the next download batch. Attempts already used still count; failed tracks are not restarted automatically.</p>
    <div class="actions">
      <button class="button primary" type="submit">{{template "icon" "checkmark-circle-02"}}Save download settings</button>
    </div>
    <div id="download-settings-feedback" class="feedback" role="status" aria-live="polite"></div>
  </form>
</section>

<section class="content-section">
  <header class="section-heading"><h2>Public proxy sources</h2></header>
  <p class="helper">Spotify and YouTube try direct access, personal proxies, then public proxies. VK uses direct access.</p>
  <form class="form" id="proxy-sources-form" action="/settings/proxy-sources?account_id={{.User.ID}}" method="post">
    <label class="field">Source URLs<textarea name="proxy_sources" spellcheck="false" autocomplete="off" aria-describedby="proxy-sources-help proxy-sources-feedback">{{.ProxySources}}</textarea></label>
    <p class="helper" id="proxy-sources-help">One HTTPS text-file URL per line. Bare IP:PORT entries use the protocol from the source filename or query, or SOCKS5 by default. Save changes before importing.</p>
  </form>
  <div class="actions">
    <button class="button primary" type="submit" form="proxy-sources-form">{{template "icon" "checkmark-circle-02"}}Save sources</button>
    <form id="public-proxy-import-form" action="/settings/public/import?account_id={{.User.ID}}" method="post">
      <button type="submit" class="button"{{if or .ProxyImport.Running (not .ProxySources)}} disabled{{end}}>{{template "icon" "reload"}}<span>Import public proxies</span></button>
    </form>
  </div>
  <div id="proxy-sources-feedback" class="feedback" role="status" aria-live="polite">{{if .SourcesSaved}}Sources saved.{{end}}</div>
  <p class="helper"><span id="public-proxy-count">{{.PublicProxyCount}}</span> public proxies saved. New proxies are saved only after a successful YouTube check.</p>
  <div id="public-proxy-import-status" class="feedback" role="status" aria-live="polite" data-running="{{.ProxyImport.Running}}">{{.ProxyImport.Message}}</div>
</section>

<section class="content-section">
  <header class="section-heading"><h2>Personal proxies</h2><button type="button" class="button" data-proxy-ping="all"{{if not .PersonalProxies}} disabled{{end}}>{{template "icon" "reload"}}<span>Ping all</span></button></header>
  <p class="helper">Ping checks YouTube access through each proxy.</p>
  <form class="form" id="personal-proxy-form" action="/settings/personal?account_id={{.User.ID}}" method="post" autocomplete="off">
    <div class="grid grid-cols-[minmax(0,1fr)_auto] items-end gap-3 max-[700px]:grid-cols-1">
      <label class="field">Proxy URL<input name="address" type="text" required autocomplete="off" autocapitalize="off" autocorrect="off" spellcheck="false" placeholder="http://login:password@host:port" aria-describedby="personal-proxy-feedback"></label>
      <div><button type="submit" class="button primary">{{template "icon" "checkmark-circle-02"}}Check and add</button></div>
    </div>
  </form>
  <div id="personal-proxy-feedback" class="feedback" role="status" aria-live="polite"></div>
  {{if .PersonalProxies}}<ul class="proxy-list">{{range .PersonalProxies}}
    <li class="proxy-row" data-proxy-id="{{.ID}}" data-proxy-host="{{.Host}}">
      <div class="proxy-identity"><span class="proxy-address" data-masked="{{.Masked}}">{{.Masked}}</span><span class="status proxy-result" role="status" aria-live="polite">{{template "icon" "circle"}}<span>{{if .CheckedAt}}Last checked {{.CheckedAt}}{{else}}Not checked{{end}}</span></span></div>
      <div class="actions">
        <button type="button" class="button" data-proxy-ping="{{.ID}}" aria-label="Ping proxy {{.Host}}">{{template "icon" "reload"}}<span>Ping</span></button>
        <button type="button" class="button responsive-action proxy-reveal" aria-expanded="false" aria-label="Show proxy URL for {{.Host}}" title="Show URL">{{template "icon" "view"}}<span class="action-label proxy-reveal-label">Show URL</span></button>
        <form class="proxy-delete" method="post" action="/settings/personal/{{.ID}}/delete?account_id={{$.User.ID}}"><button type="submit" class="button danger responsive-action" aria-label="Remove proxy {{.Host}}" title="Remove proxy">{{template "icon" "delete-02"}}<span class="action-label">Remove</span></button></form>
      </div>
    </li>
  {{end}}</ul>{{else}}<div class="empty"><strong>No personal proxies saved yet</strong></div>{{end}}
  <div id="proxy-ping-feedback" class="feedback" role="status" aria-live="polite"></div>
</section>
{{end}}`
