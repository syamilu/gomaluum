package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/bytedance/sonic"
	"github.com/gocolly/colly/v2"
	"github.com/lucsky/cuid"
	"github.com/nrmnqdds/gomaluum/internal/constants"
	"github.com/nrmnqdds/gomaluum/internal/dtos"
	"github.com/nrmnqdds/gomaluum/internal/errors"
	"github.com/nrmnqdds/gomaluum/pkg/utils"
	"github.com/rung/go-safecast"
)

// i-Ma'luum migrated /MyAcademic/schedule from a server-rendered HTML table to a
// JavaScript SPA: the page ships empty placeholders plus a <div id="schedule-app"
// data-token="..."> and the browser fetches the real data from
// /MyAcademic/schedule/data with an "X-Page-Token" header. A plain scrape of the
// page HTML therefore sees no rows. These types and helpers reproduce the SPA's
// data fetch so the scraper reads the JSON endpoint instead of the dead table.

// imaluumScheduleResponse is the envelope returned by /MyAcademic/schedule/data.
// Data is nil when i-Ma'luum has no records to show (the SPA renders a "No
// records" notice for that case).
type imaluumScheduleResponse struct {
	Data *imaluumScheduleData `json:"data"`
}

type imaluumScheduleData struct {
	MatricNo string              `json:"matric_no"`
	Ses      string              `json:"ses"`
	Sem      string              `json:"sem"`
	Subjects []imaluumRawSubject `json:"subjects"`
	AllSem   []imaluumSessionRef `json:"all_sem"`
}

type imaluumRawSubject struct {
	Code     string           `json:"code"`
	Sect     string           `json:"sect"`
	Stat     string           `json:"stat"`
	Title    string           `json:"title"`
	Chr      string           `json:"chr"`
	Schedule []imaluumRawSlot `json:"schedule"`
}

type imaluumRawSlot struct {
	Day   string `json:"day"`
	Start string `json:"start"`
	Ends  string `json:"ends"`
	Lect  string `json:"lect"`
	Venue string `json:"venue"`
}

type imaluumSessionRef struct {
	Ses string `json:"ses"`
	Sem string `json:"sem"`
}

// buildSessionQuery formats a session into the "?ses=...&sem=..." query the
// schedule page expects to select that session.
func buildSessionQuery(ses, sem string) string {
	return fmt.Sprintf("?ses=%s&sem=%s", ses, sem)
}

// buildSessionName formats a session into the "Sem <n>, <year>" label used
// throughout the app and by utils.SortSessionNames.
func buildSessionName(sem, ses string) string {
	return fmt.Sprintf("Sem %s, %s", sem, ses)
}

// sessionsFromAllSem converts the SPA's all_sem list into index-aligned session
// queries and display names, dropping the same placeholder sessions the old
// dropdown scrape filtered out.
func sessionsFromAllSem(all []imaluumSessionRef) (queries, names []string) {
	queries = make([]string, 0, len(all))
	names = make([]string, 0, len(all))
	for _, ss := range all {
		q := buildSessionQuery(ss.Ses, ss.Sem)
		if slices.Contains(UnwantedSessionQueries[:], q) {
			continue
		}
		queries = append(queries, q)
		names = append(names, buildSessionName(ss.Sem, ss.Ses))
	}
	return queries, names
}

// mapImaluumSchedule converts one session's raw SPA payload into the public
// ScheduleSubject shape. A single raw slot with a multi-day code (e.g. "T-TH")
// expands to one WeekTime per day, mirroring the old table parser. Malformed
// section/credit values default to zero rather than dropping the subject.
func mapImaluumSchedule(data *imaluumScheduleData) []dtos.ScheduleSubject {
	subjects := make([]dtos.ScheduleSubject, 0, len(data.Subjects))
	for _, raw := range data.Subjects {
		subject := dtos.ScheduleSubject{
			ID:         fmt.Sprintf("gomaluum:subject:%s", cuid.Slug()),
			CourseCode: strings.TrimSpace(raw.Code),
			CourseName: strings.TrimSpace(raw.Title),
		}

		if chr, err := strconv.ParseFloat(strings.TrimSpace(raw.Chr), 64); err == nil {
			subject.Chr = chr
		}
		if section, err := safecast.Atoi32(strings.TrimSpace(raw.Sect)); err == nil {
			subject.Section = uint32(section)
		}

		timestamps := make([]dtos.WeekTime, 0, len(raw.Schedule))
		for _, slot := range raw.Schedule {
			// Venue and lecturer sit per-slot in the new payload but per-subject in
			// the public DTO; they are consistent across a subject's slots, so take
			// the first non-empty value.
			if subject.Venue == "" {
				subject.Venue = strings.TrimSpace(slot.Venue)
			}
			if subject.Lecturer == "" {
				subject.Lecturer = strings.TrimSpace(slot.Lect)
			}

			start, startUnix := normalizeTime(slot.Start)
			end, endUnix := normalizeTime(slot.Ends)
			for _, day := range parseDays(strings.TrimSpace(slot.Day)) {
				wt := dtos.WeekTime{
					Start: start,
					End:   end,
					Day:   utils.GetScheduleDays(day),
				}
				if startUnix != nil {
					wt.StartUnix = *startUnix
				}
				if endUnix != nil {
					wt.EndUnix = *endUnix
				}
				timestamps = append(timestamps, wt)
			}
		}
		subject.Timestamps = timestamps

		subjects = append(subjects, subject)
	}
	return subjects
}

// decodeScheduleData unmarshals a /MyAcademic/schedule/data response body,
// stripping the leading UTF-8 BOM i-Ma'luum prepends. Returns a nil *data (no
// error) when the payload's "data" field is null.
func decodeScheduleData(body []byte) (*imaluumScheduleData, error) {
	body = bytes.TrimPrefix(body, []byte("\ufeff"))
	var resp imaluumScheduleResponse
	if err := sonic.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// fetchPageToken loads the schedule page for the given session query and returns
// the single-use X-Page-Token minted for that render. An empty token with no
// error means the page was fetched but carried no token (login bounce or a page
// shape change); callers check stale to disambiguate.
func (s *Server) fetchPageToken(ctx context.Context, cookie, query string, stale *atomic.Bool) (string, error) {
	var token string
	c := s.newImaluumCollector(ctx, cookie, stale)
	c.OnHTML("#schedule-app", func(e *colly.HTMLElement) {
		token = e.Attr("data-token")
	})
	if err := c.Visit(constants.ImaluumSchedulePage + query); err != nil {
		return "", classifyVisitError(err)
	}
	return token, nil
}

// fetchScheduleData reproduces the SPA's data fetch for one session: load the
// page to mint a fresh X-Page-Token, then GET the JSON data endpoint with it.
// Returns a nil *data (no error) when the session has no records. A nil return
// with stale set means the session cookie expired; the caller retries.
func (s *Server) fetchScheduleData(ctx context.Context, cookie, query string, stale *atomic.Bool) (*imaluumScheduleData, error) {
	token, err := s.fetchPageToken(ctx, cookie, query, stale)
	if err != nil {
		return nil, err
	}
	if stale.Load() {
		return nil, nil
	}
	if token == "" {
		// Page loaded but no token: the SPA markup changed. Surface it rather than
		// silently returning an empty schedule.
		return nil, errors.ErrScheduleIsEmpty
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, constants.ImaluumScheduleDataPage, nil)
	if err != nil {
		return nil, errors.Wrap(errors.ErrFailedToGoToURL, err)
	}
	req.Header.Set("Cookie", "MOD_AUTH_CAS="+cookie)
	req.Header.Set("User-Agent", constants.DefaultUserAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Page-Token", token)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(errors.ErrFailedToGoToURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return nil, errors.ErrUpstreamForbidden
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Wrap(errors.ErrFailedToGoToURL, fmt.Errorf("schedule data endpoint returned %d", resp.StatusCode))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(errors.ErrFailedToGoToURL, err)
	}
	return decodeScheduleData(body)
}
