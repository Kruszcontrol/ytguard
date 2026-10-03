# Home Assistant integration

There are two ways to connect YTGuard PCs to Home Assistant:

- **MQTT (recommended).** Each PC shows up in Home Assistant automatically, with sensors, switches and buttons and no YAML. One automation handles approvals for every PC. Needs an MQTT broker; most Home Assistant installs already have the Mosquitto add-on.
- **Webhook + REST API.** No broker needed, but you configure sensors and buttons in YAML for each PC. See [Without MQTT](#without-mqtt-webhook--rest-api).

Both can be used at the same time.

## MQTT setup

### 1. Broker and a user for YTGuard

1. In Home Assistant: **Settings → Add-ons → Mosquitto broker**. Install it if it isn't already, and make sure the **MQTT** integration is set up.
2. Create a login for YTGuard. Either add a Home Assistant user (**Settings → People → Users**, e.g. `ytguard`; it doesn't need admin rights), or add it under the Mosquitto add-on's **Configuration → Logins**.
   One login can be shared by all PCs, or use one per PC so you can revoke them separately.
   Don't allow anonymous access to the broker: anyone who can publish to it can press YTGuard's buttons.

### 2. Each YTGuard PC

On each PC's YTGuard page: **Settings → Home Assistant**:

- tick **MQTT**;
- **Broker address**: your Home Assistant's address (e.g. `homeassistant.local` or its IP), **port** `1883` (or `8883` with **Use TLS**);
- the **username** and **password** from step 1;
- tick **Send instant events**, and under **Daily report** tick **Home Assistant** if you want reports in HA;
- **Save**. The status line should change to "connected since …" within a few seconds.

Each PC gets its own ID (shown next to the status), so any number of PCs can share one broker without clashing.

### 3. What appears in Home Assistant

Under **Settings → Devices & services → MQTT** you'll find:

**One device per PC**, "YTGuard <PC name>":

| Entity | What it is |
|---|---|
| Pending requests | number of "Ask a parent" requests waiting |
| Other browsers found | browsers/video apps YTGuard found on that PC |
| YTGuard (update) | shows in **Settings → Updates** when a new release is out (install it on the PC with `sudo ytguard upgrade`) |
| Events | fires on approval requests, time up, other browsers found, updates, daily reports, logins |

**One device per kid**, "<Kid> YouTube", linked to their PC:

| Entity | What it is |
|---|---|
| Time used today / Time left today | minutes ("Time left" is unavailable if the day is unlimited) |
| Break in | minutes until the next enforced break (unavailable if no breaks) |
| YouTube state | `watching`, `idle`, `break`, `time_up`, `locked`, `outside_window` or `none_today` |
| Watching | the current video's title (attributes: channel, link) |
| Watching now | on while a video is playing |
| Videos today | count |
| Pause YouTube | switch: on = YouTube paused for the rest of the day |
| Add 15 minutes / Add 30 minutes / End break | buttons |

If a PC is switched off or YTGuard stops, its entities show as unavailable.

### 4. Phone notifications and approvals (blueprint)

Import the blueprint (one automation covers every PC):

[![Import blueprint](https://my.home-assistant.io/badges/blueprint_import.svg)](https://my.home-assistant.io/redirect/blueprint_import/?blueprint_url=https%3A%2F%2Fgithub.com%2FKruszcontrol%2Fytguard%2Fblob%2Fmain%2Fdocs%2Fblueprints%2Fytguard_notifications.yaml)

Or **Settings → Automations & scenes → Blueprints → Import blueprint** with
`https://github.com/Kruszcontrol/ytguard/blob/main/docs/blueprints/ytguard_notifications.yaml`.

Create an automation from it, pick your phone, and choose which other alerts you want (time up, other browsers, updates, daily reports).
When a kid asks to watch something, your phone shows the video with **Allow video**, **Allow channel** and **Deny** buttons. The answer goes straight back to the PC the request came from.

### Example dashboard card

```yaml
type: entities
title: Alice — YouTube
entities:
  - entity: sensor.alice_youtube_youtube_state
  - entity: sensor.alice_youtube_watching
  - entity: sensor.alice_youtube_time_used_today
  - entity: sensor.alice_youtube_time_left_today
  - entity: switch.alice_youtube_pause_youtube
  - entity: button.alice_youtube_add_15_minutes
  - entity: button.alice_youtube_end_break
```

(Entity IDs are made from the device and entity names; check them under the device page.)

### MQTT topics (for your own automations)

`<prefix>` is `ytguard` unless changed; `<pc>` is the PC's ID.

| Topic | Direction | Content |
|---|---|---|
| `<prefix>/<pc>/status` | YTGuard → | `online` / `offline` |
| `<prefix>/<pc>/state` | YTGuard → | PC JSON: `pc`, `version`, `pending_requests`, `other_apps` |
| `<prefix>/<pc>/kid/<id>/state` | YTGuard → | kid JSON: `used_minutes`, `remaining_minutes`, `state`, `watching`, `video_title`, `locked`, … |
| `<prefix>/<pc>/event` | YTGuard → | `{"event_type": "approval_request", "kid": …, "approve_topic": …}` etc. |
| `<prefix>/<pc>/kid/<id>/cmd` | → YTGuard | `bonus:15`, `lock:60` (0 = rest of today), `unlock`, `end_break` |
| `<prefix>/<pc>/kid/<id>/pause/set` | → YTGuard | `ON` / `OFF` |
| `<prefix>/<pc>/request/<id>/set` | → YTGuard | `approve_video`, `approve_channel`, `deny` |

Event payloads have the same fields as the webhook payloads below, plus `event_type` and `pc_id`.

## Without MQTT: webhook + REST API

YTGuard talks to Home Assistant in two directions:

| Direction | How | Used for |
|---|---|---|
| YTGuard → HA | HA **webhook** (secret URL) | daily reports, approval requests, "time's up", parent logins, other browsers found, updates |
| HA → YTGuard | REST API with a scoped **API token** | sensors (time used/left, watching now, pending requests), bonus time, pause, approve/deny |

Each kid PC runs its own YTGuard, so repeat the steps below per PC (examples use a PC called `kidpc`).
With several PCs, MQTT is much less work.

### 1. Webhook (YTGuard → HA)

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

### 2. API token (HA → YTGuard)

In YTGuard: **Security → API tokens**, name it "Home Assistant", keep **read** and **control** ticked (leave **admin** off unless you want HA to edit filters). Copy the token — it's shown once.

`secrets.yaml`:

```yaml
ytguard_kidpc_auth: "Bearer ytg_PASTE_TOKEN_HERE"
```

### 3. Sensors and commands

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

### 4. Automations

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

## Opening the full YTGuard UI from Home Assistant

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
