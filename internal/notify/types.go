package notify

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
)

type TargetStatus struct {
	Name, State             string
	Checked, Snapshot, Next time.Time
}
type Fault struct {
	Object, Code             string
	First, Recovered, NextAt time.Time
}
type Event struct {
	Kind, Timezone, Source string
	At                     time.Time
	Count, Undelivered     uint64
	Faults, ActiveFaults   map[string]Fault
	Targets                []TargetStatus
}

func Add(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}
func (e Event) clone() Event {
	e.Targets = slices.Clone(e.Targets)
	e.Faults = maps.Clone(e.Faults)
	e.ActiveFaults = maps.Clone(e.ActiveFaults)
	return e
}

// Delivery failures coalesce the newest status and incident history into one message.
func (e *Event) Merge(next Event) {
	if next.Count == 0 {
		return
	}
	count := Add(e.Count, next.Count)
	missed := Add(e.Undelivered, next.Undelivered)
	faults := maps.Clone(e.Faults)
	if faults == nil {
		faults = map[string]Fault{}
	}
	for key, f := range next.Faults {
		if old, ok := faults[key]; ok && old.First.Before(f.First) && old.Recovered.IsZero() {
			f.First = old.First
		}
		faults[key] = f
	}
	*e = next.clone()
	e.Faults, e.Count, e.Undelivered = faults, count, missed
}
func (e Event) Title() string {
	return "Vaultwarden backup — " + e.Kind
}
func (e Event) PlainSummary() string {
	protected := 0
	for _, t := range e.Targets {
		if t.State == "protected" {
			protected++
		}
	}
	return fmt.Sprintf("SQLite %s. Repositories protected: %d/%d.", e.Source, protected, len(e.Targets))
}
func markdownText(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, value)
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\\", "\\\\", "*", "\\*", "_", "\\_", "`", "\\`", "[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "#", "\\#", "!", "\\!", "|", "\\|", "~", "\\~", "-", "\\-", "+", "\\+", ".", "\\.").Replace(value)
}
func (e Event) time(t time.Time) string {
	if t.IsZero() {
		return "pending"
	}
	loc, err := time.LoadLocation(e.Timezone)
	if err != nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04:05 -07:00")
}
func (e Event) Message(limit int) string {
	message := "**" + e.Title() + "**\n" + e.time(e.At) + "\n" + e.PlainSummary()
	if e.Undelivered > 0 {
		message += fmt.Sprintf("\nPreviously undelivered reports: %d", e.Undelivered)
	}
	omitted := 0
	appendLine := func(line string) {
		if len(message)+len(line)+32 > limit {
			omitted++
			return
		}
		message += "\n\n" + line
	}
	faults := maps.Clone(e.ActiveFaults)
	if faults == nil {
		faults = map[string]Fault{}
	}
	maps.Copy(faults, e.Faults)
	for _, key := range slices.Sorted(maps.Keys(faults)) {
		f := faults[key]
		line := markdownText(f.Object) + " · " + markdownText(f.Code) + " · since " + e.time(f.First)
		if !f.Recovered.IsZero() {
			line += " · recovered " + e.time(f.Recovered)
		}
		appendLine(line)
	}
	for _, t := range e.Targets {
		line := markdownText(t.Name) + " · " + markdownText(t.State) + " · checked " + e.time(t.Checked) + " · snapshot " + e.time(t.Snapshot)
		if !t.Next.IsZero() {
			line += " · retry " + e.time(t.Next)
		}
		appendLine(line)
	}
	if omitted > 0 {
		message += fmt.Sprintf("\nOmitted details: %d", omitted)
	}
	return message
}
