package main

import (
	"bytes"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"

	"golang.org/x/net/html"

	"launchpad.net/xmlpath"
)

const (
	CalendarURL = "http://www.trivalleytriclub.com/calendar"

	// Timezone for calendar, all events on Pacific time
	Timezone = "America/Los_Angeles"

	// Time format, see RFC 2445 Sec 4.3.5
	ICalTimeFormat = "20060102T150405Z"

	// XPaths to various things of interest
	TRPath     = `//div[@id="main"]/table/tbody/tr`
	MonthXpath = `//div[@id="main"]/table/caption`
	TDPath     = `./td`
)

// Default timezone Location
var Location *time.Location

// Template for the output, an ical file
const ICalTemplate = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Tri-Valley Triathlon Club//trivalleytriclub.com//
METHOD:PUBLISH
{{range .}}BEGIN:VEVENT
TRANSP:TRANSPARENT
DTSTART:{{.Start}}
DTEND:{{.End}}
SUMMARY:{{.Summary}}
LOCATION:{{.Location}}
UID:{{.Start}}-{{.End}}@trivalleytriclub.com
SEQUENCE:0
DTSTAMP:{{now}}
END:VEVENT
{{end}}END:VCALENDAR`

type Workout struct {
	Summary  string
	Location string
	Start    string
	End      string
}

var (
	testFile = flag.String("test", "", "test using a predownloaded HTML file")
	outFile  = flag.String("out", "tvtc.ical", "output file")
)

// fixHTML cleans up messy HTML before running it through xmlpath which expects
// cleaner HTML.
func fixHTML(reader io.Reader) (*xmlpath.Node, error) {
	var buf bytes.Buffer

	root, err := html.Parse(reader)
	if err != nil {
		log.Fatal(err)
	}

	html.Render(&buf, root)

	return xmlpath.ParseHTML(&buf)
}

// parseMonth extracts the month from the caption inside the main table
func parseMonth(n *xmlpath.Node) time.Month {
	val, ok := xmlpath.MustCompile(MonthXpath).String(n)
	if !ok {
		log.Fatal("failed to find month")
	}

	month := strings.TrimSpace(strings.Split(val, " ")[0])
	for i := 1; i < 12; i++ {
		if time.Month(i).String() == month {
			return time.Month(i)
		}
	}

	log.Fatalf("invalid month: `%s`", month)
	return time.January
}

// parseDayOfMonth finds the number in the first TD of a TR containing days of
// the month.
func parseDayOfMonth(n *xmlpath.Node) int {
	val, ok := xmlpath.MustCompile(TDPath).String(n)
	if !ok {
		log.Fatal("failed to find day")
	}

	parts := strings.Split(strings.TrimSpace(val), " ")
	d, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		log.Fatalf("failed to parse day: `%s`", val)
	}

	return d
}

// parseWorkoutRow handles a TR containing workouts. Increments base by one day
// per TD as each TD contains all the workouts for a single day.
func parseWorkoutRow(base *time.Time, n *xmlpath.Node) []*Workout {
	path := xmlpath.MustCompile(TDPath)

	workouts := []*Workout{}

	iter := path.Iter(n)
	for iter.Next() {
		workouts = append(workouts, parseWorkouts(*base, iter.Node())...)
		*base = base.Add(24 * time.Hour)
	}

	return workouts
}

// parseWorkouts handles all workouts for a single day. Extracts information
// into Workout structs.
func parseWorkouts(base time.Time, n *xmlpath.Node) []*Workout {
	var workouts []*Workout

	lines := strings.Split(n.String(), "\n")
	for len(lines) >= 11 {
		loc := []string{}
		for i := 0; i < 3; i++ {
			loc = append(loc, strings.TrimSpace(lines[3+2*i]))
		}

		parts := strings.Split(strings.TrimSpace(lines[9]), " ")
		if len(parts) != 2 {
			log.Printf("unexpected number of parts in time: `%s`", lines[9])

			return nil
		}

		hourmins := strings.Split(parts[0], ":")
		if len(hourmins) != 2 {
			log.Printf("unable to parse time as duration: `%s`", parts[0])
			return nil
		}
		hour, err := strconv.Atoi(hourmins[0])
		min, err2 := strconv.Atoi(hourmins[1])
		if err != nil || err2 != nil {
			log.Printf("unable to parse time as duration: `%s`", parts[0])
			return nil
		}

		if parts[1] == "PM" {
			hour += 12
		} else if parts[1] != "AM" {
			log.Printf("expected AM/PM and not: `%s`", parts[1])
			return nil
		}

		// Create the precise start date so that it should handle daylight savings
		start := time.Date(
			base.Year(), base.Month(), base.Day(), // Only care about date from base
			hour, min, // Parsed from HTML
			0, 0, // Seconds/nanoseconds
			Location,
		)

		workouts = append(workouts, &Workout{
			Summary:  strings.TrimSpace(lines[2]),
			Location: strings.TrimSpace(strings.Join(loc, ", ")),
			Start:    start.UTC().Format(ICalTimeFormat),
			End:      start.Add(time.Minute * 90).UTC().Format(ICalTimeFormat),
		})

		// Chop off already processed workout
		lines = lines[10:]
	}

	return workouts
}

// writeCalendar renders the workouts using the ICalTemplate to fname.
func writeCalendar(fname string, workouts []*Workout) error {
	fns := template.FuncMap{
		"now": func() string {
			return time.Now().UTC().Format(ICalTimeFormat)
		},
	}

	f, err := os.Create(fname)
	if err != nil {
		return err
	}
	defer f.Close()

	tmpl, err := template.New("ical").Funcs(fns).Parse(ICalTemplate)
	if err != nil {
		return err
	}

	return tmpl.Execute(f, workouts)
}

// parseCalendar takes a parsed HTML tree and extracts all the workouts from
// the main table.
func parseCalendar(root *xmlpath.Node) ([]*Workout, error) {
	var err error
	var base time.Time
	var workouts []*Workout

	path := xmlpath.MustCompile(TRPath)

	now := time.Now()

	month := parseMonth(root)

	year := now.Year()
	if month == time.December && now.Month() == time.January {
		// On last week of the year
		year -= 1
	}

	Location, err = time.LoadLocation(Timezone)
	if err != nil {
		return nil, err
	}

	iter := path.Iter(root)
	for i := 0; iter.Next(); i++ {
		node := iter.Node()

		if i == 0 {
			day := parseDayOfMonth(node)
			base = time.Date(year, month, day, 0, 0, 0, 0, Location)
		}

		if i%2 == 1 {
			workouts = append(workouts, parseWorkoutRow(&base, node)...)
		}
	}

	return workouts, nil
}

func main() {
	flag.Parse()

	var r io.Reader
	var err error

	if *testFile != "" {
		r, err = os.Open(*testFile)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		log.Printf("downloading %s", CalendarURL)

		resp, err := http.Get(CalendarURL)
		if err != nil {
			log.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			log.Fatalf("unable to fetch calendar, status code: %d", resp.StatusCode)
		}

		r = resp.Body
	}

	root, err := fixHTML(r)
	if err != nil {
		log.Fatal(err)
	}

	workouts, err := parseCalendar(root)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("parsed %d workouts", len(workouts))

	if err := writeCalendar(*outFile, workouts); err != nil {
		log.Fatal(err)
	}
}
