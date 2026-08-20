package config

import (
	"strings"
	"testing"
)

const (
	validGeneral = `[general]
timezone = "Europe/Paris"
default_preset = "p"
retention_days = 30
`
	validPicker = `[picker]
hour_min = 8
hour_max = 18
minute_step = 15
default_time = "09:00"
week_start = "monday"
`
	validPresets = `[[preset]]
key = "p"
label = "P"
kind = "offset"
offset = "30m"
`
)

// doc assembles a config document, substituting the valid default for any
// empty part, so each test case states only what it breaks.
func doc(general, picker, presets string) string {
	if general == "" {
		general = validGeneral
	}
	if picker == "" {
		picker = validPicker
	}
	if presets == "" {
		presets = validPresets
	}
	return general + "\n" + picker + "\n" + presets
}

func TestValidationRules(t *testing.T) {
	preset := func(body string) string { return "[[preset]]\n" + body }
	tests := []struct {
		name    string
		doc     string
		wantErr []string // all must appear in the error
	}{
		{
			"duplicate key",
			doc("", "", validPresets+validPresets),
			[]string{`"p"`, "duplicate"},
		},
		{
			"empty key",
			doc("", "", preset(`key = ""
label = "P"
kind = "offset"
offset = "30m"
`)),
			[]string{"empty key"},
		},
		{
			"empty label",
			doc("", "", preset(`key = "p"
label = ""
kind = "offset"
offset = "30m"
`)),
			[]string{`"p"`, "empty label"},
		},
		{
			"default_preset not matching a key",
			doc(`[general]
timezone = "Europe/Paris"
default_preset = "nope"
retention_days = 30
`, "", ""),
			[]string{"default_preset", `"nope"`},
		},
		{
			"empty preset list",
			validGeneral + "\n" + validPicker,
			[]string{"preset list", "empty"},
		},
		{
			"cross-kind field: weekday carrying offset",
			doc("", "", preset(`key = "p"
label = "P"
kind = "weekday"
weekday = "monday"
at = "09:00"
offset = "30m"
`)),
			[]string{`"p"`, `"offset"`, `"weekday"`},
		},
		{
			"cross-kind field: clock carrying same_day_ok",
			doc("", "", preset(`key = "p"
label = "P"
kind = "clock"
at = "09:00"
same_day_ok = true
`)),
			[]string{`"p"`, `"same_day_ok"`, `"clock"`},
		},
		{
			"cross-kind field: offset carrying at",
			doc("", "", preset(`key = "p"
label = "P"
kind = "offset"
offset = "30m"
at = "09:00"
`)),
			[]string{`"p"`, `"at"`, `"offset"`},
		},
		{
			"missing required field for kind",
			doc("", "", preset(`key = "p"
label = "P"
kind = "offset"
`)),
			[]string{`"p"`, "requires", "offset"},
		},
		{
			"unparseable offset",
			doc("", "", preset(`key = "p"
label = "P"
kind = "offset"
offset = "banana"
`)),
			[]string{`"p"`, `"banana"`},
		},
		{
			"zero offset",
			doc("", "", preset(`key = "p"
label = "P"
kind = "offset"
offset = "0m"
`)),
			[]string{`"p"`, `"0m"`, "positive"},
		},
		{
			"negative offset",
			doc("", "", preset(`key = "p"
label = "P"
kind = "offset"
offset = "-30m"
`)),
			[]string{`"p"`, `"-30m"`, "positive"},
		},
		{
			"negative days",
			doc("", "", preset(`key = "p"
label = "P"
kind = "clock"
at = "09:00"
days = -1
`)),
			[]string{`"p"`, "-1"},
		},
		{
			"at not HH:MM",
			doc("", "", preset(`key = "p"
label = "P"
kind = "clock"
at = "9:00"
`)),
			[]string{`"p"`, `"9:00"`, "HH:MM"},
		},
		{
			"at out of 24-hour range",
			doc("", "", preset(`key = "p"
label = "P"
kind = "clock"
at = "25:00"
`)),
			[]string{`"p"`, `"25:00"`},
		},
		{
			"unknown weekday",
			doc("", "", preset(`key = "p"
label = "P"
kind = "weekday"
weekday = "funday"
at = "09:00"
`)),
			[]string{`"p"`, `"funday"`},
		},
		{
			"unknown kind",
			doc("", "", preset(`key = "p"
label = "P"
kind = "cron"
`)),
			[]string{`"p"`, `"cron"`},
		},
		{
			// LoadLocation("") returns UTC with a nil error, so a deleted
			// timezone line would otherwise pass validation and schedule
			// every clock/weekday preset in UTC silently.
			"empty timezone",
			doc(`[general]
timezone = ""
default_preset = "p"
retention_days = 30
`, "", ""),
			[]string{"timezone"},
		},
		{
			"missing timezone line",
			doc(`[general]
default_preset = "p"
retention_days = 30
`, "", ""),
			[]string{"timezone"},
		},
		{
			"Local is not a stated timezone",
			doc(`[general]
timezone = "Local"
default_preset = "p"
retention_days = 30
`, "", ""),
			[]string{"timezone", "Local"},
		},
		{
			"unknown timezone",
			doc(`[general]
timezone = "Mars/Olympus"
default_preset = "p"
retention_days = 30
`, "", ""),
			[]string{"timezone", `"Mars/Olympus"`},
		},
		{
			"week_start not monday or sunday",
			doc("", `[picker]
hour_min = 8
hour_max = 18
minute_step = 15
default_time = "09:00"
week_start = "tuesday"
`, ""),
			[]string{"week_start", `"tuesday"`},
		},
		{
			"hour_min outside 0..23",
			doc("", `[picker]
hour_min = -1
hour_max = 18
minute_step = 15
default_time = "09:00"
week_start = "monday"
`, ""),
			[]string{"hour_min", "-1"},
		},
		{
			"hour_max outside 0..23",
			doc("", `[picker]
hour_min = 8
hour_max = 24
minute_step = 15
default_time = "09:00"
week_start = "monday"
`, ""),
			[]string{"hour_max", "24"},
		},
		{
			"hour_min greater than hour_max",
			doc("", `[picker]
hour_min = 18
hour_max = 8
minute_step = 15
default_time = "18:00"
week_start = "monday"
`, ""),
			[]string{"hour_min", "18", "8"},
		},
		{
			"minute_step zero",
			doc("", `[picker]
hour_min = 8
hour_max = 18
minute_step = 0
default_time = "09:00"
week_start = "monday"
`, ""),
			[]string{"minute_step", "0"},
		},
		{
			"minute_step not dividing 60",
			doc("", `[picker]
hour_min = 8
hour_max = 18
minute_step = 7
default_time = "09:00"
week_start = "monday"
`, ""),
			[]string{"minute_step", "7"},
		},
		{
			"negative retention_days",
			doc(`[general]
timezone = "Europe/Paris"
default_preset = "p"
retention_days = -1
`, "", ""),
			[]string{"retention_days", "-1"},
		},
		{
			"default_time not HH:MM",
			doc("", `[picker]
hour_min = 8
hour_max = 18
minute_step = 15
default_time = "9am"
week_start = "monday"
`, ""),
			[]string{"default_time", `"9am"`},
		},
		{
			"default_time outside picker hours",
			doc("", `[picker]
hour_min = 8
hour_max = 18
minute_step = 15
default_time = "07:00"
week_start = "monday"
`, ""),
			[]string{"default_time", `"07:00"`},
		},
		{
			"default_time off the minute_step grid",
			doc("", `[picker]
hour_min = 8
hour_max = 18
minute_step = 15
default_time = "09:07"
week_start = "monday"
`, ""),
			[]string{"default_time", `"09:07"`},
		},
		{
			"unknown key in a preset",
			doc("", "", validPresets+"quik = true\n"),
			[]string{"unknown key", "quik"},
		},
		{
			"unknown section",
			doc("", "", "") + "\n[pickr]\nhour_min = 8\n",
			[]string{"unknown key", "pickr"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parse([]byte(tt.doc))
			if err == nil {
				t.Fatal("parse accepted the document, want a validation error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestValidationAccepts(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{"the assembled valid document", doc("", "", "")},
		// §5.4: [picker] constrains only the sheet — a preset at 07:00
		// under hour_min = 8 is valid.
		{"preset at outside picker hours", doc(`[general]
timezone = "Europe/Paris"
default_preset = "early"
retention_days = 30
`, "", `[[preset]]
key = "early"
label = "Early"
kind = "clock"
at = "07:00"
`)},
		{"clock days defaulting to 0", doc(`[general]
timezone = "Europe/Paris"
default_preset = "today"
retention_days = 30
`, "", `[[preset]]
key = "today"
label = "Today"
kind = "clock"
at = "23:00"
`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parse([]byte(tt.doc)); err != nil {
				t.Errorf("parse rejected a valid document: %v", err)
			}
		})
	}
}

func TestParseHHMM(t *testing.T) {
	good := map[string][2]int{
		"00:00": {0, 0}, "09:05": {9, 5}, "23:59": {23, 59},
	}
	for s, want := range good {
		h, m, err := ParseHHMM(s)
		if err != nil || h != want[0] || m != want[1] {
			t.Errorf("ParseHHMM(%q) = %d, %d, %v; want %d, %d", s, h, m, err, want[0], want[1])
		}
	}
	for _, s := range []string{"9:00", "24:00", "12:60", "12-30", "1200", "12:3a", ""} {
		if _, _, err := ParseHHMM(s); err == nil {
			t.Errorf("ParseHHMM(%q) accepted, want error", s)
		}
	}
}
