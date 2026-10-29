package notify

import (
	"fmt"
	"github.com/hellodword/vaultwarden-less/internal/routes"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
)

type TargetStatus struct {
	Name, State                           string
	Waiting, Uploading, Failed            bool
	LastUpload, LastComplete, Next        time.Time
	Structure, ReadData                   string
	StructureAt, ReadDataAt               time.Time
	StructureNext, ReadDataNext           time.Time
	StructureScheduled, ReadDataScheduled bool
}
type Fault struct {
	Object      string
	Category    string
	Code        string
	First       time.Time
	Last        time.Time
	Recovered   time.Time
	Occurrences uint64
	Details     map[string]int
	CompleteAt  time.Time
	NextAt      time.Time
}
type Event struct {
	Kind                                string
	At                                  time.Time
	Timezone                            string
	PeriodStart, PeriodEnd              time.Time
	Checks, Changes, Count, Undelivered uint64
	Snapshots                           map[string]uint64
	Faults                              map[string]Fault
	ActiveFaults                        map[string]Fault
	Reasons                             map[string]uint64
	Routes                              map[string]routes.Counts
	Unknown                             map[string]routes.Counts
	Source                              string
	Issues                              map[string]int
	Targets                             []TargetStatus
	Warnings                            []string
}

func Add(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}
func cloneFaults(in map[string]Fault) map[string]Fault {
	out := maps.Clone(in)
	for key, f := range out {
		f.Details = maps.Clone(f.Details)
		out[key] = f
	}
	return out
}
func (e Event) clone() Event {
	e.Reasons = maps.Clone(e.Reasons)
	e.Routes = maps.Clone(e.Routes)
	e.Unknown = maps.Clone(e.Unknown)
	e.Issues = maps.Clone(e.Issues)
	e.Targets = slices.Clone(e.Targets)
	e.Warnings = slices.Clone(e.Warnings)
	e.Faults = cloneFaults(e.Faults)
	e.ActiveFaults = cloneFaults(e.ActiveFaults)
	e.Snapshots = maps.Clone(e.Snapshots)
	return e
}

// Merge retains one summary per fault, including an incident recovered before
// its first delivery. It does not keep an unbounded event log.
func (e *Event) Merge(next Event) {
	e.Count = Add(e.Count, next.Count)
	e.Checks = Add(e.Checks, next.Checks)
	e.Changes = Add(e.Changes, next.Changes)
	e.Undelivered = Add(e.Undelivered, next.Undelivered)
	if next.Kind != "" {
		e.Kind = next.Kind
	}
	if !next.At.IsZero() {
		e.At = next.At
	}
	if next.Timezone != "" {
		e.Timezone = next.Timezone
	}
	if !next.PeriodStart.IsZero() && (e.PeriodStart.IsZero() || next.PeriodStart.Before(e.PeriodStart)) {
		e.PeriodStart = next.PeriodStart
	}
	if next.PeriodEnd.After(e.PeriodEnd) {
		e.PeriodEnd = next.PeriodEnd
	}
	if e.Snapshots == nil {
		e.Snapshots = map[string]uint64{}
	}
	for name, n := range next.Snapshots {
		e.Snapshots[name] = Add(e.Snapshots[name], n)
	}
	if e.Faults == nil {
		e.Faults = map[string]Fault{}
	}
	for key, f := range next.Faults {
		if old, ok := e.Faults[key]; ok {
			if old.First.Before(f.First) {
				f.First = old.First
			}
			f.Occurrences = max(old.Occurrences, f.Occurrences)
		}
		f.Details = maps.Clone(f.Details)
		e.Faults[key] = f
	}
	if next.ActiveFaults != nil {
		e.ActiveFaults = cloneFaults(next.ActiveFaults)
	}
	if e.Reasons == nil {
		e.Reasons = map[string]uint64{}
	}
	for key, n := range next.Reasons {
		e.Reasons[key] = Add(e.Reasons[key], n)
	}
	activity := routes.Summary{Routes: e.Routes, Unknown: e.Unknown}
	activity.Merge(routes.Summary{Routes: next.Routes, Unknown: next.Unknown})
	e.Routes, e.Unknown = activity.Routes, activity.Unknown
	if next.Source != "" {
		e.Source = next.Source
		e.Issues = maps.Clone(next.Issues)
		e.Warnings = slices.Clone(next.Warnings)
	}
	for _, t := range next.Targets {
		index := slices.IndexFunc(e.Targets, func(old TargetStatus) bool { return old.Name == t.Name })
		if index >= 0 {
			e.Targets[index] = t
		} else {
			e.Targets = append(e.Targets, t)
		}
	}
}
func (e Event) Title() string {
	kind := e.Kind
	if kind == "" {
		kind = "Daily"
	}
	return "Vaultwarden backup — " + kind
}
func (e Event) sourceDescription() string {
	switch e.Source {
	case "complete":
		return "Complete"
	case "incomplete":
		return "Incomplete; retry pending"
	case "failed":
		return "Capture failed; retry pending"
	case "unconfigured":
		return "Unconfigured"
	default:
		return "Not checked yet"
	}
}
func (e Event) targetSummary() string {
	failed, pending := 0, 0
	for _, t := range e.Targets {
		if t.Failed || t.State == "failed" || t.State == "invalid" {
			failed++
		}
		if t.Waiting || t.Uploading {
			pending++
		}
	}
	return fmt.Sprintf("%d configured; %d failed; %d pending", len(e.Targets), failed, pending)
}
func (e Event) PlainSummary() string {
	return "Source: " + e.sourceDescription() + ". Targets: " + e.targetSummary() + "."
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
		return "none"
	}
	location, err := time.LoadLocation(e.Timezone)
	if err != nil {
		location = time.UTC
	}
	return t.In(location).Format("2006-01-02 15:04:05 -07:00")
}
func (e Event) Message(limit int) string {
	header := "**" + e.Title() + "**\nTime: " + e.time(e.At)
	if !e.PeriodStart.IsZero() {
		header += "\nPeriod: " + e.time(e.PeriodStart) + " to " + e.time(e.PeriodEnd)
	}
	header += "\n\nSource: " + e.sourceDescription() + "\nTargets: " + e.targetSummary()
	header += fmt.Sprintf("\nVerification: %d unresolved faults", countVerification(e.ActiveFaults))
	footer := fmt.Sprintf("\n\nSource checks: %d; effective changes: %d", e.Checks, e.Changes)
	if e.Undelivered > 0 {
		footer += fmt.Sprintf("\nPreviously undelivered reports: %d", e.Undelivered)
	}
	var details []string
	for _, key := range slices.Sorted(maps.Keys(e.Faults)) {
		f := e.Faults[key]
		line := "Faults / Recoveries: " + markdownText(f.Object) + "; " + markdownText(f.Category) + "; " + markdownText(f.Code) + "; first " + e.time(f.First)
		if !f.Recovered.IsZero() {
			line += "; occurred then recovered " + e.time(f.Recovered) + "; duration " + f.Recovered.Sub(f.First).String()
		}
		line += "; latest complete " + e.time(f.CompleteAt) + "; next action " + e.time(f.NextAt)
		details = append(details, line)
	}
	for _, key := range slices.Sorted(maps.Keys(e.ActiveFaults)) {
		if _, ok := e.Faults[key]; !ok {
			f := e.ActiveFaults[key]
			details = append(details, "Unresolved: "+markdownText(f.Object)+"; "+markdownText(f.Category)+"; "+markdownText(f.Code)+"; first "+e.time(f.First)+"; next "+e.time(f.NextAt))
		}
	}
	for _, t := range e.Targets {
		details = append(details, "Targets: "+markdownText(t.Name)+"; "+markdownText(t.State)+fmt.Sprintf("; snapshots %d", e.Snapshots[t.Name])+"; latest upload "+e.time(t.LastUpload)+"; latest complete "+e.time(t.LastComplete))
		schedule := func(enabled bool, next time.Time) string {
			if !enabled {
				return "unscheduled"
			}
			return "next " + e.time(next)
		}
		details = append(details, "Verification: "+markdownText(t.Name)+"; structure "+markdownText(t.Structure)+" at "+e.time(t.StructureAt)+", "+schedule(t.StructureScheduled, t.StructureNext)+"; full read "+markdownText(t.ReadData)+" at "+e.time(t.ReadDataAt)+", "+schedule(t.ReadDataScheduled, t.ReadDataNext))
		if !t.Next.IsZero() {
			details = append(details, "Next actions: "+markdownText(t.Name)+" retry "+e.time(t.Next))
		}
	}
	for _, code := range slices.Sorted(maps.Keys(e.Issues)) {
		details = append(details, fmt.Sprintf("Source issue: %s (%d)", markdownText(code), e.Issues[code]))
	}
	for _, warning := range e.Warnings {
		details = append(details, "Source warning: "+markdownText(warning))
	}
	activity := func(prefix string, counts map[string]routes.Counts) {
		for _, name := range slices.Sorted(maps.Keys(counts)) {
			count := counts[name]
			details = append(details, fmt.Sprintf("%s%s: started %d, finished %d", prefix, markdownText(name), count.Started, count.Finished))
		}
	}
	activity("Unknown ", e.Unknown)
	activity("", e.Routes)
	message, omitted := header, 0
	for _, detail := range details {
		block := "\n\n" + detail
		if len(message)+len(block)+len(footer)+len(fmt.Sprintf("\nOmitted details: %d", len(details))) > limit {
			omitted++
			continue
		}
		message += block
	}
	message += footer
	if omitted > 0 {
		message += fmt.Sprintf("\nOmitted details: %d", omitted)
	}
	return message
}
func countVerification(faults map[string]Fault) int {
	count := 0
	for _, f := range faults {
		if f.Category == "structure" || f.Category == "read_data" {
			count++
		}
	}
	return count
}
