package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"time"

	"yggpeers/internal/store"
)

// statusString renders a peer's up/down state plus how long it has been in
// that state. It's exposed to templates as the {{status}} func.
func statusString(p store.PeerWithStats) string {
	state := "online"
	if !p.IsUp {
		state = "offline"
	}
	if p.StateChangedAt.IsZero() {
		return state
	}
	d := p.StateDuration()
	switch {
	case d >= 7*24*time.Hour:
		return state + " 1 week+"
	case d >= 2*24*time.Hour:
		return fmt.Sprintf("%s %d days", state, int(d.Hours()/24))
	case d >= 24*time.Hour:
		return state + " 1 day"
	case d >= 2*time.Hour:
		return fmt.Sprintf("%s %d hours", state, int(d.Hours()))
	case d >= time.Hour:
		return state + " 1 hour"
	case d >= 2*time.Minute:
		return fmt.Sprintf("%s %d minutes", state, int(d.Minutes()))
	case d >= time.Minute:
		return state + " 1 minute"
	default:
		return state + " just now"
	}
}

// uptimeString formats a peer's rolling uptime percentage. Exposed to
// templates as the {{uptime}} func.
func uptimeString(pct *float64) string {
	if pct == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *pct)
}

// percentString formats a 0-100 float as "12.3". Exposed as {{pct}}.
func percentString(v float64) string {
	return fmt.Sprintf("%.1f", v)
}

// optPercentString formats an optional percentage as "12.3%" or "-".
// Exposed as {{optpct}}.
func optPercentString(pct *float64) string {
	if pct == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *pct)
}

// jsonString marshals a value to JSON for embedding in templates.
func jsonString(v any) template.JS {
	b, _ := json.Marshal(v)
	return template.JS(b)
}
