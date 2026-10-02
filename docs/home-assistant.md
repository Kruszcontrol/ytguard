# Home Assistant integration

YTGuard talks to Home Assistant in two directions:

| Direction | How | Used for |
|---|---|---|
| YTGuard → HA | HA **webhook** (secret URL) | daily reports, approval requests, "time's up", parent logins, other browsers found, updates |
| HA → YTGuard | REST API with a scoped **API token** | sensors (time used/left, watching now, pending requests), bonus time, pause, approve/deny |

Each kid PC runs its own YTGuard, so repeat the steps below per PC (examples use a PC called `kidpc`).

## 1. Webhook (YTGuard → HA)

1. Pick a long random webhook ID, e.g. `ytguard-kidpc-8f3k2m9q7x`.
2. In YTGuard: **Settings → Home Assistant → Webhook URL**:
   `http://homeassistant.local:8123/api/webhook/ytguard-kidpc-8f3k2m9q7x`
   Tick **Send instant events**, and under **Daily report** tick **Home Assistant**.
   If HA uses HTTPS with a self-signed certificate, tick the "self-signed" box.
3. Press **Send test to Home Assistant** after adding the automation below.

Payloads (all have `type`, `pc`, `time`):

- `daily_report`: `summary`, `text` (plain-text report), `report` (`kid`, `date`, `total_minutes`, `limit_minutes`, `videos[]`, `blocked[]`, `hidden[]`, `requests[]`)
- `approval_request`: `kid`, `request_id`, `title`, `channel`, `message`, `reason`, `url`, `review_url`
- `time_up`: `kid`, `used_minutes`
- `admin_login`: `device`, `ip`
- `update_available`: `current`, `latest`, `url` (release notes), `notes`, `how` (sent once per new release)
- `unapproved_app`: `kid` ("Everyone on this PC" for system-wide installs), `app`, `kind` (`running`/`installed`), `how`, `where`, `message` (sent once per new finding)
- `test`: `summary`

## 2. API token (HA → YTGuard)

In YTGuard: **Security → API tokens**, name it "Home Assistant", keep **read** and **control** ticked (leave **admin** off unless you want HA to edit filters). Copy the token — it's shown once.

`secrets.yaml`:

```yaml
ytguard_kidpc_auth: "Bearer ytg_PASTE_TOKEN_HERE"
```

## 3. Sensors and commands

`configuration.yaml` (replace `Alice` with the kid's name as shown in YTGuard, and the host with your kid PC's address):

```yaml
rest:
  - resource: https://kidpc.local:8443/api/v1/state
    verify_ssl: false            # YTGuard uses a self-signed certificate
    headers:
      Authorization: !secret ytguard_kidpc_auth
    scan_interval: 60
    sensor:
      - name: "Alice YouTube used"
        unit_of_measurement: min
        value_template: "{{ (value_json.kids | selectattr('name','eq','Alice') | first).used_minutes }}"
      - name: "Alice YouTube left"
        unit_of_measurement: min
        value_template: "{{ (value_json.kids | selectattr('name','eq','Alice') | first).remaining_minutes }}"
      - name: "Alice YouTube state"   # watching | idle | break | time_up | locked | outside_window | none_today
        value_template: "{{ (value_json.kids | selectattr('name','eq','Alice') | first).state }}"
      - name: "Alice YouTube video"
        value_template: "{{ (value_json.kids | selectattr('name','eq','Alice') | first).video_title | default('') }}"
      - name: "YTGuard kidpc pending requests"
        value_template: "{{ value_json.pending_requests }}"
      - name: "YTGuard kidpc version"
        value_template: "{{ value_json.ytguard.version }}"
        json_attributes_path: "$.ytguard"
        json_attributes: [latest_version, update_available, release_url, update_checked]
      - name: "YTGuard kidpc other browsers"   # other browsers / video apps found
        value_template: "{{ value_json.other_apps }}"

rest_command:
  ytguard_kidpc_bonus:
    url: "https://kidpc.local:8443/api/v1/kids/{{ kid }}/bonus"
    method: POST
    verify_ssl: false
    headers:
      Authorization: !secret ytguard_kidpc_auth
    content_type: application/json
    payload: '{"minutes": {{ minutes }}}'
  ytguard_kidpc_lock:          # minutes: 0 = rest of today
    url: "https://kidpc.local:8443/api/v1/kids/{{ kid }}/lock"
    method: POST
    verify_ssl: false
    headers:
      Authorization: !secret ytguard_kidpc_auth
    content_type: application/json
    payload: '{"minutes": {{ minutes }}}'
  ytguard_kidpc_unlock:
    url: "https://kidpc.local:8443/api/v1/kids/{{ kid }}/unlock"
    method: POST
    verify_ssl: false
    headers:
      Authorization: !secret ytguard_kidpc_auth
  ytguard_kidpc_approve:       # scope: video | channel
    url: "https://kidpc.local:8443/api/v1/requests/{{ id }}/approve"
    method: POST
    verify_ssl: false
    headers:
      Authorization: !secret ytguard_kidpc_auth
    content_type: application/json
    payload: '{"scope": "{{ scope | default(''video'') }}"}'
  ytguard_kidpc_deny:
    url: "https://kidpc.local:8443/api/v1/requests/{{ id }}/deny"
    method: POST
    verify_ssl: false
    headers:
      Authorization: !secret ytguard_kidpc_auth
```

Example script call: `action: rest_command.ytguard_kidpc_bonus` with `data: {kid: Alice, minutes: 15}`.

## 4. Automations

Replace `notify.mobile_app_your_phone` with your phone's notify service.

```yaml
automation:
  - alias: "YTGuard kidpc events"
    triggers:
      - trigger: webhook
        webhook_id: ytguard-kidpc-8f3k2m9q7x
        allowed_methods: [POST]
        local_only: true
    actions:
      - choose:
          - conditions: "{{ trigger.json.type == 'daily_report' }}"
            sequence:
              - action: notify.mobile_app_your_phone
                data:
                  title: "YouTube: {{ trigger.json.report.kid }}"
                  message: "{{ trigger.json.summary }}"
              - action: persistent_notification.create
                data:
                  title: "YouTube report {{ trigger.json.report.kid }} {{ trigger.json.report.date }}"
                  message: "{{ trigger.json.text }}"
          - conditions: "{{ trigger.json.type == 'approval_request' }}"
            sequence:
              - action: notify.mobile_app_your_phone
                data:
                  title: "{{ trigger.json.kid }} asks to watch"
                  message: >-
                    {{ trigger.json.title }} ({{ trigger.json.channel }})
                    {% if trigger.json.message %} — “{{ trigger.json.message }}”{% endif %}
                  data:
                    url: "{{ trigger.json.url }}"
                    actions:
                      - action: "YTG_KIDPC_APPROVE_{{ trigger.json.request_id }}"
                        title: "Allow video"
                      - action: "YTG_KIDPC_CHANNEL_{{ trigger.json.request_id }}"
                        title: "Allow channel"
                      - action: "YTG_KIDPC_DENY_{{ trigger.json.request_id }}"
                        title: "Deny"
          - conditions: "{{ trigger.json.type == 'update_available' }}"
            sequence:
              - action: persistent_notification.create
                data:
                  title: "YTGuard update for {{ trigger.json.pc }}"
                  message: >-
                    {{ trigger.json.latest }} is available (installed: {{ trigger.json.current }}).
                    [Release notes]({{ trigger.json.url }}). To install, run on that PC: `sudo ytguard upgrade`
          - conditions: "{{ trigger.json.type == 'unapproved_app' }}"
            sequence:
              - action: notify.mobile_app_your_phone
                data:
                  title: "Other browser found on {{ trigger.json.pc }}"
                  message: "{{ trigger.json.message }}"
          - conditions: "{{ trigger.json.type in ['time_up', 'admin_login', 'test'] }}"
            sequence:
              - action: notify.mobile_app_your_phone
                data:
                  title: "YTGuard {{ trigger.json.pc }}"
                  message: >-
                    {% if trigger.json.type == 'time_up' %}{{ trigger.json.kid }} used all {{ trigger.json.used_minutes }} min today.
                    {% elif trigger.json.type == 'admin_login' %}New parent login from {{ trigger.json.device }} ({{ trigger.json.ip }}).
                    {% else %}{{ trigger.json.summary }}{% endif %}

  - alias: "YTGuard kidpc approve/deny from notification"
    triggers:
      - trigger: event
        event_type: mobile_app_notification_action
    conditions: "{{ trigger.event.data.action.startswith('YTG_KIDPC_') }}"
    actions:
      - variables:
          parts: "{{ trigger.event.data.action.split('_') }}"
          verb: "{{ parts[2] }}"
          rid: "{{ parts[3] }}"
      - choose:
          - conditions: "{{ verb == 'DENY' }}"
            sequence:
              - action: rest_command.ytguard_kidpc_deny
                data: { id: "{{ rid }}" }
        default:
          - action: rest_command.ytguard_kidpc_approve
            data:
              id: "{{ rid }}"
              scope: "{{ 'channel' if verb == 'CHANNEL' else 'video' }}"
```

## 5. Opening the full YTGuard UI from Home Assistant

**Recommended:** a dashboard button that opens the UI in the browser. Your phone's browser keeps the
"remember this device" login.

```yaml
type: button
name: YTGuard (kidpc)
icon: mdi:youtube
tap_action:
  action: url
  url_path: https://kidpc.local:8443/
```

**Optional:** to show the UI inside HA with a *Webpage* card, add HA's address (e.g. `https://homeassistant.local:8123`) under
**Settings → Allow showing this UI inside these sites**. Notes:

- If HA is served over HTTPS, the browser will refuse an iframe with an untrusted self-signed certificate. Open the YTGuard
  URL directly once and accept the certificate, or give YTGuard a trusted certificate.
- The login cookie is then sent as a third-party (partitioned) cookie. Some browsers and the HA phone apps block those, so
  you may have to log in again inside the card, or it may not work at all. If so, use the button.
