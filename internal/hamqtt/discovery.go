package hamqtt

import (
	"fmt"
	"sort"
	"strings"

	"ytguard"
	"ytguard/internal/mqtt"
	"ytguard/internal/store"
)

// entity is one Home Assistant MQTT discovery config.
type entity struct {
	component string // sensor, binary_sensor, switch, button, update, event
	objectID  string // unique within this PC
	config    map[string]any
}

const discoveredKey = "mqtt_discovered" // settings key: config topics we published

// publishDiscovery (re)announces all entities and removes ones that no
// longer exist (e.g. a kid was deleted).
func (b *Bridge) publishDiscovery(c *mqtt.Client, base, disc string) error {
	ents, kidKey := b.entities(base)
	current := map[string]bool{}
	for _, e := range ents {
		topic := fmt.Sprintf("%s/%s/ytguard_%s_%s/config", disc, e.component, b.PCID, e.objectID)
		current[topic] = true
		if err := publishJSON(c, topic, e.config, true); err != nil {
			return err
		}
	}
	var old []string
	_, _ = b.App.St.GetJSON(discoveredKey, &old)
	for _, t := range old {
		if !current[t] {
			if err := c.Publish(t, nil, true); err != nil { // empty retained config deletes the entity
				return err
			}
		}
	}
	var list []string
	for t := range current {
		list = append(list, t)
	}
	sort.Strings(list)
	_ = b.App.St.SetJSON(discoveredKey, list)
	b.mu.Lock()
	b.kidKeyPublished = kidKey
	b.mu.Unlock()
	return nil
}

// kidsChanged reports whether kids were added, removed or renamed since the
// last discovery.
func (b *Bridge) kidsChanged(_ *mqtt.Client) bool {
	kids, err := b.App.St.Kids()
	if err != nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return kidKey(kids) != b.kidKeyPublished
}

func (b *Bridge) entities(base string) ([]entity, string) {
	s, _ := b.App.St.Settings()
	kids, _ := b.App.St.Kids()
	pcDevice := map[string]any{
		"identifiers":  []string{"ytguard_" + b.PCID},
		"name":         "YTGuard " + firstNonEmpty(s.PCName, b.PCID),
		"manufacturer": "YTGuard",
		"model":        "Family YouTube controls",
		"sw_version":   ytguard.Version,
	}
	if strings.HasPrefix(s.PublicURL, "https://") || strings.HasPrefix(s.PublicURL, "http://") {
		pcDevice["configuration_url"] = s.PublicURL
	}
	avail := []map[string]any{{"topic": base + "/status"}}
	uid := func(obj string) string { return "ytguard_" + b.PCID + "_" + obj }
	ent := func(component, obj string, device map[string]any, cfg map[string]any) entity {
		cfg["unique_id"] = uid(obj)
		cfg["device"] = device
		if _, ok := cfg["availability"]; !ok {
			cfg["availability"] = avail
		}
		return entity{component, obj, cfg}
	}
	out := []entity{
		ent("sensor", "pending_requests", pcDevice, map[string]any{"name": "Pending requests", "icon": "mdi:account-question",
			"state_topic": base + "/state", "value_template": "{{ value_json.pending_requests }}", "state_class": "measurement"}),
		ent("sensor", "other_apps", pcDevice, map[string]any{"name": "Other browsers found", "icon": "mdi:web-cancel",
			"state_topic": base + "/state", "value_template": "{{ value_json.other_apps }}", "state_class": "measurement"}),
		ent("update", "update", pcDevice, map[string]any{"name": "YTGuard", "state_topic": base + "/update",
			"entity_category": "diagnostic"}),
		ent("event", "events", pcDevice, map[string]any{"name": "Events", "state_topic": base + "/event",
			"event_types": EventTypes, "icon": "mdi:bell-ring"}),
	}
	for _, k := range kids {
		kb := fmt.Sprintf("%s/kid/%d", base, k.ID)
		obj := func(s string) string { return fmt.Sprintf("kid%d_%s", k.ID, s) }
		dev := map[string]any{
			"identifiers": []string{fmt.Sprintf("ytguard_%s_kid%d", b.PCID, k.ID)},
			"name":        k.Name + " YouTube",
			"via_device":  "ytguard_" + b.PCID,
			"model":       "YTGuard kid on " + firstNonEmpty(s.PCName, b.PCID),
		}
		state := kb + "/state"
		// Entities that make no sense for unlimited time / no breaks show
		// as unavailable then.
		availWhen := func(field string) []map[string]any {
			return []map[string]any{{"topic": base + "/status"},
				{"topic": state, "value_template": "{{ 'online' if value_json." + field + " is not none else 'offline' }}"}}
		}
		out = append(out,
			ent("sensor", obj("used"), dev, map[string]any{"name": "Time used today", "icon": "mdi:timer-sand",
				"state_topic": state, "value_template": "{{ value_json.used_minutes }}", "unit_of_measurement": "min", "state_class": "measurement"}),
			ent("sensor", obj("left"), dev, map[string]any{"name": "Time left today", "icon": "mdi:timer-outline",
				"state_topic": state, "value_template": "{{ value_json.remaining_minutes }}", "unit_of_measurement": "min",
				"state_class": "measurement", "availability": availWhen("remaining_minutes"), "availability_mode": "all"}),
			ent("sensor", obj("break_in"), dev, map[string]any{"name": "Break in", "icon": "mdi:coffee-outline",
				"state_topic": state, "value_template": "{{ value_json.break_in_minutes }}", "unit_of_measurement": "min",
				"availability": availWhen("break_in_minutes"), "availability_mode": "all"}),
			ent("sensor", obj("state"), dev, map[string]any{"name": "YouTube state", "icon": "mdi:youtube-tv",
				"state_topic": state, "value_template": "{{ value_json.state }}", "device_class": "enum",
				"options":               []string{"watching", "idle", "break", "time_up", "locked", "outside_window", "none_today"},
				"json_attributes_topic": state, "json_attributes_template": "{{ {'message': value_json.message, 'limit_minutes': value_json.limit_minutes} | tojson }}"}),
			ent("sensor", obj("video"), dev, map[string]any{"name": "Watching", "icon": "mdi:youtube",
				"state_topic": state, "value_template": "{{ value_json.video_title if value_json.watching else 'nothing' }}",
				"json_attributes_topic": state, "json_attributes_template": "{{ {'channel': value_json.video_channel, 'url': value_json.video_url} | tojson }}"}),
			ent("sensor", obj("videos_today"), dev, map[string]any{"name": "Videos today", "icon": "mdi:counter",
				"state_topic": state, "value_template": "{{ value_json.today_videos }}", "state_class": "measurement"}),
			ent("binary_sensor", obj("watching"), dev, map[string]any{"name": "Watching now", "icon": "mdi:play-circle",
				"state_topic": state, "value_template": "{{ 'ON' if value_json.watching else 'OFF' }}"}),
			ent("switch", obj("pause"), dev, map[string]any{"name": "Pause YouTube", "icon": "mdi:pause-octagon",
				"state_topic": state, "value_template": "{{ 'ON' if value_json.locked else 'OFF' }}",
				"command_topic": kb + "/pause/set", "payload_on": "ON", "payload_off": "OFF"}),
			ent("button", obj("bonus15"), dev, map[string]any{"name": "Add 15 minutes", "icon": "mdi:timer-plus-outline",
				"command_topic": kb + "/cmd", "payload_press": "bonus:15"}),
			ent("button", obj("bonus30"), dev, map[string]any{"name": "Add 30 minutes", "icon": "mdi:timer-plus",
				"command_topic": kb + "/cmd", "payload_press": "bonus:30"}),
			ent("button", obj("end_break"), dev, map[string]any{"name": "End break", "icon": "mdi:coffee-off",
				"command_topic": kb + "/cmd", "payload_press": "end_break"}),
		)
	}
	return out, kidKey(kids)
}

func kidKey(kids []store.Kid) string {
	var parts []string
	for _, k := range kids {
		parts = append(parts, fmt.Sprintf("%d=%s", k.ID, k.Name))
	}
	return strings.Join(parts, ",")
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
