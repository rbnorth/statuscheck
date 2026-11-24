// statuscheck checks the public status pages for GitHub, Bitbucket and Azure.
//
// GitHub and Bitbucket publish their status through Atlassian Statuspage,
// which exposes a JSON summary at /api/v2/summary.json. Azure publishes an
// RSS feed that lists only active incidents (it's empty when all is well).
//
// Usage:
//
//	go run main.go               # summary + any degraded components / incidents
//	go run main.go -s github     # only GitHub (or -s bitbucket, -s azure, -s all)
//	go run main.go -v            # list every component
//	go run main.go -json         # machine-readable output
//
// Exit codes: 0 = all operational, 1 = degraded/incident, 2 = check failed.
package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Feed formats the tool knows how to read.
const (
	kindStatuspage = "statuspage" // Atlassian Statuspage summary.json
	kindAzureRSS   = "azure-rss"  // Azure status RSS feed
)

type service struct {
	Key  string // value used with the -s flag
	Name string
	URL  string
	Kind string
	Page string // human-facing status page
}

var services = []service{
	{Key: "github", Name: "GitHub", Kind: kindStatuspage,
		URL: "https://www.githubstatus.com/api/v2/summary.json"},
	{Key: "bitbucket", Name: "Bitbucket", Kind: kindStatuspage,
		URL: "https://bitbucket.status.atlassian.com/api/v2/summary.json"},
	{Key: "azure", Name: "Azure", Kind: kindAzureRSS,
		URL:  "https://azure.status.microsoft/en-us/status/feed/",
		Page: "https://azure.status.microsoft/en-us/status/"},
}

// defaultServices is what runs when -s isn't given.
// Change to "github" once Bitbucket no longer matters.
const defaultServices = "all"

// selectServices turns "github", "bitbucket", "github,bitbucket" or "all"
// into the matching list of services.
func selectServices(spec string) ([]service, error) {
	spec = strings.ToLower(strings.TrimSpace(spec))
	if spec == "" || spec == "all" || spec == "both" {
		return services, nil
	}
	var out []service
	seen := map[string]bool{}
	for _, want := range strings.Split(spec, ",") {
		want = strings.TrimSpace(want)
		if want == "" || seen[want] {
			continue
		}
		found := false
		for _, s := range services {
			if s.Key == want {
				out = append(out, s)
				found = true
				break
			}
		}
		if !found {
			keys := make([]string, len(services))
			for i, s := range services {
				keys[i] = s.Key
			}
			return nil, fmt.Errorf("unknown service %q (choose from: %s, or all)", want, strings.Join(keys, ", "))
		}
		seen[want] = true
	}
	return out, nil
}

// Statuspage summary.json (only the fields we use).
type summary struct {
	Page struct {
		Name      string    `json:"name"`
		URL       string    `json:"url"`
		UpdatedAt time.Time `json:"updated_at"`
	} `json:"page"`
	Status struct {
		Indicator   string `json:"indicator"` // none, minor, major, critical, maintenance
		Description string `json:"description"`
	} `json:"status"`
	Components []struct {
		Name   string `json:"name"`
		Status string `json:"status"` // operational, degraded_performance, partial_outage, major_outage, under_maintenance
		Group  bool   `json:"group"`
	} `json:"components"`
	Incidents             []incident `json:"incidents"`
	ScheduledMaintenances []struct {
		Name           string    `json:"name"`
		Status         string    `json:"status"`
		ScheduledFor   time.Time `json:"scheduled_for"`
		ScheduledUntil time.Time `json:"scheduled_until"`
	} `json:"scheduled_maintenances"`
}

type incident struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Impact    string    `json:"impact"`
	Shortlink string    `json:"shortlink"`
	UpdatedAt time.Time `json:"updated_at"`
}

type result struct {
	Service string   `json:"service"`
	Summary *summary `json:"summary,omitempty"`
	Err     string   `json:"error,omitempty"`
}

// Azure status RSS feed (only the fields we use). The feed has no <item>
// elements when there are no active incidents.
type azureRSS struct {
	Channel struct {
		Title         string `xml:"title"`
		Link          string `xml:"link"`
		LastBuildDate string `xml:"lastBuildDate"`
		Items         []struct {
			Title    string   `xml:"title"`
			Link     string   `xml:"link"`
			PubDate  string   `xml:"pubDate"`
			Category []string `xml:"category"`
		} `xml:"item"`
	} `xml:"channel"`
}

// parseRSSTime handles the date formats seen in RSS feeds, including
// Azure's "Sun, 22 Nov 2026 22:58:00 Z". Returns the zero time on failure.
func parseRSSTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		time.RFC1123Z,
		time.RFC1123,
		"Mon, 02 Jan 2006 15:04:05 Z07:00",
		"Mon, 2 Jan 2006 15:04:05 Z07:00",
		"Mon, 2 Jan 2006 15:04:05 MST",
		"Mon, 2 Jan 2006 15:04:05 -0700",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// azureToSummary converts the Azure RSS feed into the same shape as a
// Statuspage summary, so printing, JSON output and exit codes work the same.
func azureToSummary(feed azureRSS, s service) *summary {
	var sum summary
	sum.Page.Name = s.Name
	sum.Page.URL = s.Page
	if feed.Channel.Link != "" {
		sum.Page.URL = feed.Channel.Link
	}
	sum.Page.UpdatedAt = parseRSSTime(feed.Channel.LastBuildDate)

	for _, it := range feed.Channel.Items {
		inc := incident{
			Name:      strings.TrimSpace(it.Title),
			Shortlink: strings.TrimSpace(it.Link),
			UpdatedAt: parseRSSTime(it.PubDate),
		}
		if len(it.Category) > 0 {
			inc.Status = strings.Join(it.Category, ", ")
		}
		sum.Incidents = append(sum.Incidents, inc)
	}

	switch n := len(sum.Incidents); n {
	case 0:
		sum.Status.Indicator = "none"
		sum.Status.Description = "No active incidents"
	case 1:
		sum.Status.Indicator = "major"
		sum.Status.Description = "1 active incident"
	default:
		sum.Status.Indicator = "major"
		sum.Status.Description = fmt.Sprintf("%d active incidents", n)
	}
	return &sum
}

func fetch(ctx context.Context, client *http.Client, s service) result {
	r := result{Service: s.Name}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	if s.Kind == kindAzureRSS {
		req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", "statuscheck/1.0")

	resp, err := client.Do(req)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		r.Err = fmt.Sprintf("unexpected HTTP status %s", resp.Status)
		return r
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		r.Err = fmt.Sprintf("reading response: %v", err)
		return r
	}

	switch s.Kind {
	case kindAzureRSS:
		var feed azureRSS
		if err := xml.Unmarshal(body, &feed); err != nil {
			r.Err = fmt.Sprintf("decoding RSS: %v", err)
			return r
		}
		r.Summary = azureToSummary(feed, s)
	default:
		var sum summary
		if err := json.Unmarshal(body, &sum); err != nil {
			r.Err = fmt.Sprintf("decoding response: %v", err)
			return r
		}
		r.Summary = &sum
	}
	return r
}

// ANSI colors, disabled when NO_COLOR is set or stdout isn't a terminal.
var useColor = func() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}()

func color(code, s string) string {
	if !useColor {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func indicatorColor(ind string) string {
	switch ind {
	case "none", "operational":
		return "32" // green
	case "minor", "degraded_performance", "maintenance", "under_maintenance":
		return "33" // yellow
	default:
		return "31" // red
	}
}

func pretty(s string) string { return strings.ReplaceAll(s, "_", " ") }

func printResult(r result, verbose bool) {
	fmt.Println(color("1", r.Service))
	if r.Err != "" {
		fmt.Printf("  %s %s\n\n", color("31", "✗ check failed:"), r.Err)
		return
	}
	s := r.Summary

	mark := "✓"
	if s.Status.Indicator != "none" {
		mark = "!"
	}
	fmt.Printf("  %s %s\n", color(indicatorColor(s.Status.Indicator), mark), s.Status.Description)

	// Components: all with -v, otherwise only those not operational.
	comps := s.Components
	sort.SliceStable(comps, func(i, j int) bool { return comps[i].Name < comps[j].Name })
	for _, c := range comps {
		if c.Group {
			continue
		}
		if !verbose && c.Status == "operational" {
			continue
		}
		fmt.Printf("    - %-40s %s\n", c.Name, color(indicatorColor(c.Status), pretty(c.Status)))
	}

	for _, inc := range s.Incidents {
		var details []string
		if inc.Status != "" {
			details = append(details, pretty(inc.Status))
		}
		if inc.Impact != "" {
			details = append(details, "impact: "+inc.Impact)
		}
		extra := ""
		if len(details) > 0 {
			extra = " [" + strings.Join(details, ", ") + "]"
		}
		fmt.Printf("  %s %s%s\n", color("31", "Incident:"), inc.Name, extra)
		if inc.Shortlink != "" {
			fmt.Printf("      %s\n", inc.Shortlink)
		}
	}

	for _, m := range s.ScheduledMaintenances {
		if m.Status == "completed" {
			continue
		}
		fmt.Printf("  %s %s [%s] %s → %s\n", color("33", "Maintenance:"), m.Name, pretty(m.Status),
			m.ScheduledFor.Local().Format("Jan 2 15:04"), m.ScheduledUntil.Local().Format("Jan 2 15:04 MST"))
	}

	if s.Page.UpdatedAt.IsZero() {
		fmt.Printf("  %s\n\n", s.Page.URL)
	} else {
		fmt.Printf("  updated %s · %s\n\n", s.Page.UpdatedAt.Local().Format(time.RFC1123), s.Page.URL)
	}
}

func main() {
	timeout := flag.Duration("timeout", 10*time.Second, "request timeout per service")
	verbose := flag.Bool("v", false, "show every component, not just degraded ones")
	asJSON := flag.Bool("json", false, "output raw results as JSON")
	which := flag.String("s", defaultServices, "services to check, comma-separated: github, bitbucket, azure, or all")
	flag.Parse()

	selected, err := selectServices(*which)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client := &http.Client{}

	results := make([]result, len(selected))
	var wg sync.WaitGroup
	for i, s := range selected {
		wg.Add(1)
		go func(i int, s service) {
			defer wg.Done()
			results[i] = fetch(ctx, client, s)
		}(i, s)
	}
	wg.Wait()

	exit := 0
	for _, r := range results {
		switch {
		case r.Err != "":
			exit = 2
		case r.Summary.Status.Indicator != "none" && exit == 0:
			exit = 1
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
	} else {
		for _, r := range results {
			printResult(r, *verbose)
		}
	}
	os.Exit(exit)
}
