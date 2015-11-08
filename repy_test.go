package repy

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestTimeOfDayToString(t *testing.T) {
	testCases := []struct {
		x    TimeOfDay
		want string
	}{
		{0, "00:00"},
		{60, "01:00"},
		{90, "01:30"},
	}

	for _, tc := range testCases {
		got := tc.x.String()
		if got != tc.want {
			t.Errorf("%v.String() == %q; want %q", tc.x, got, tc.want)
		}
	}
}

func TestTimeOfDayFromString(t *testing.T) {
	testCases := []struct {
		x    string
		want TimeOfDay
	}{
		{"6.30", 6*60 + 30},
		{" 6.30", 6*60 + 30},
		{"16.30", 16*60 + 30},
		{"16.00", 16 * 60},
	}

	for _, tc := range testCases {
		got, err := parseTimeOfDay(tc.x)
		if err != nil {
			t.Errorf("parseTimeOfDay(%q) -> %v", tc.x, err)
		} else if got != tc.want {
			t.Errorf("parseTimeOfDay(%q) == %s; want %s", tc.x, got, tc.want)
		}
	}
}

func TestParseTestDate(t *testing.T) {
	testCases := []struct {
		data string
		want Date
	}{
		{"|             11/02/16 'ה  םוי: ןושאר דעומ |", Date{2016, 02, 11}},
		{"|             08/03/16 'ג  םוי:   ינש דעומ |", Date{2016, 03, 8}},
	}

	for i, tc := range testCases {
		cp := newCourseParserFromString(tc.data, fmt.Sprintf("parseTestDate%d", i))
		got, ok := cp.getTestDateFromLine(tc.data)
		if !ok {
			t.Errorf("getTestDateFromLine(%q) -> NOT OK", tc.data)
		} else if got != tc.want {
			t.Errorf("getTestDateFromLine(%q) == %v; want %v", tc.data, got, tc.want)
		}
	}
}

func TestParseCourse(t *testing.T) {
	testCases := []struct {
		data string
		want Course
	}{
		{`
+------------------------------------------+
|                עדימ ןוסחא תוכרעמ  234322 |
|3.0 :קנ          1-ת 2-ה:עובשב הארוה תועש |
+------------------------------------------+
|             11/02/16 'ה  םוי: ןושאר דעומ |
|                              ----------- |
|             08/03/16 'ג  םוי:   ינש דעומ |
|                              ----------- |
|               ++++++                  .סמ|
|                                     םושיר|
|      בואט 009  10.30-12.30'ג :האצרה      |
|                רגדי.ג    ר"ד : הצרמ      |
|                               -----      |
|                                          |
|      בואט 005  17.30-18.30'ג :ליגרת  11  |
|                                          |
|      בואט 006  15.30-16.30'ד :ליגרת  12  |
|                                          |
|                     -        :ליגרת  13  |
+------------------------------------------+
`, Course{
			id:             234322,
			name:           "מערכות אחסון מידע",
			academicPoints: 3.0,
			weeklyHours:    WeeklyHours{lecture: 2, tutorial: 1},
			testDates: []Date{
				Date{2016, 2, 11},
				Date{2016, 3, 8},
			},
		}},
	}

	for i, tc := range testCases {
		cp := newCourseParserFromString(strings.TrimSpace(tc.data),
			fmt.Sprintf("testParseCourse%d", i))
		got, err := cp.parse()
		if err != nil {
			t.Errorf("Error parsing course: %v\n%s", err, tc.data)
		} else if !reflect.DeepEqual(*got, tc.want) {
			t.Errorf("Mismatch parsing course. Course data:\n%s\nGot:\n%+v\nWant:\n%+v",
				"" /* TODO(lutzky): strings.TrimSpace(tc.data)*/, *got, tc.want)
		}
	}
}
