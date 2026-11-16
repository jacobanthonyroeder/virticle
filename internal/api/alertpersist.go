package api

import (
	"encoding/json"
	"log"
	"time"

	"virticle/internal/store"
)

// What the alert pass remembers is kept in the database (table web_settings, key alertStateKey), so a restart of the
// controller neither sends every open alert again nor forgets the clear of one that ended meanwhile. It holds the
// open alerts and the guests that may still be frozen. It is written after a pass only when it changed.

const alertStateKey = "alert_state"

type alertState struct {
	Active []Alert       `json:"active"`
	Frozen []frozenGuest `json:"frozen,omitempty"`
}

// saveAlertState writes the tracker's open alerts and the frozen-guest records; last is the text written the time
// before, so an unchanged state costs nothing. It returns the text now stored.
func saveAlertState(st *store.Store, tr *alertTracker, last *string) {
	if st == nil {
		return
	}
	s := alertState{Frozen: allFrozen()}
	for _, a := range tr.active {
		s.Active = append(s.Active, a)
	}
	sortAlerts(s.Active)
	b, _ := json.Marshal(s)
	if last != nil && *last == string(b) {
		return
	}
	if err := st.SetWebSetting(alertStateKey, string(b)); err != nil {
		log.Printf("alerts: saving the state: %v", err)
		return
	}
	if last != nil {
		*last = string(b)
	}
}

// restoreAlertState gives a new tracker the open alerts of the one before the restart, and the frozen-guest records
// their place again. A node-down alert brings back a time that is past the delay, so that the node, if it is still
// away, keeps its alert and does not wait for the delay again. It returns the text read.
func restoreAlertState(st *store.Store, tr *alertTracker, now time.Time) string {
	if st == nil {
		return ""
	}
	v, ok, err := st.WebSetting(alertStateKey)
	if err != nil || !ok {
		return ""
	}
	var s alertState
	if json.Unmarshal([]byte(v), &s) != nil {
		return ""
	}
	for _, a := range s.Active {
		tr.active[a.ID] = a
		if a.Kind == "node-down" && a.Node != 0 {
			tr.offlineSince[a.Node] = time.Unix(a.Since, 0).Add(-tr.cfg.NodeDown - time.Second) // it was raised after that delay
		}
	}
	setFrozen(s.Frozen)
	return v
}
