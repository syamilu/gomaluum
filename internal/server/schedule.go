package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/lucsky/cuid"
	"github.com/nrmnqdds/gomaluum/internal/constants"
	"github.com/nrmnqdds/gomaluum/internal/dtos"
	"github.com/nrmnqdds/gomaluum/internal/errors"
	"github.com/nrmnqdds/gomaluum/pkg/utils"

	_ "time/tzdata"
)

var UnwantedSessionQueries = [...]string{
	"?ses=1111/1111&sem=1",
	"?ses=0000/0000&sem=0",
}

// Pre-map day conversions for better performance
var dayMap = map[string][]string{
	"MTW":    {"M", "T", "W"},
	"TWTH":   {"T", "W", "TH"},
	"MTWTH":  {"M", "T", "W", "TH"},
	"MTWTHF": {"M", "T", "W", "TH", "F"},
}

// Worker pool structures
type scheduleJob struct {
	query string
	name  string
}

type scheduleResult struct {
	err      error
	schedule dtos.ScheduleResponse
}

// Fast day parsing using pre-built map
func parseDays(dayStr string) []string {
	cleaned := strings.ReplaceAll(dayStr, " ", "")
	if mapped, exists := dayMap[cleaned]; exists {
		return mapped
	}
	return strings.Split(cleaned, "-")
}

// Normalize time format efficiently
func normalizeTime(timeStr string) (string, *int64) {
	trimmed := strings.TrimSpace(timeStr)

	if len(trimmed) == 3 {
		trimmed = fmt.Sprintf("0%s", trimmed) // Pad single-digit times
	}

	now := time.Now()

	KLTimezone, err := time.LoadLocation("Asia/Kuala_Lumpur")
	if err != nil {
		fmt.Println("Error parsing time:", err)
		return trimmed, nil
	}

	t, err := time.ParseInLocation("2006-01-02 1504", fmt.Sprintf("%04d-%02d-%02d %s", now.Year(), now.Month(), now.Day(), trimmed), KLTimezone)
	if err != nil {
		fmt.Println("Error parsing time:", err)
		return trimmed, nil
	}

	unixTimestamp := t.Unix()

	return trimmed, &unixTimestamp
}

// Worker function for processing schedule sessions. Each job fetches one
// session's data through the i-Ma'luum schedule SPA endpoint (see schedule_spa.go)
// and maps it into a ScheduleResponse. A stale session yields nil data; the
// worker emits an empty response and the handler retries after re-login.
func (s *Server) scheduleWorker(ctx context.Context, jobs <-chan scheduleJob, results chan<- scheduleResult, cookie string, stale *atomic.Bool) {
	for job := range jobs {
		func() {
			defer utils.CatchPanic("schedule worker")

			data, err := s.fetchScheduleData(ctx, cookie, job.query, stale)
			if err != nil {
				results <- scheduleResult{err: err}
				return
			}

			subjects := []dtos.ScheduleSubject{}
			if data != nil {
				subjects = mapImaluumSchedule(data)
			}

			results <- scheduleResult{
				schedule: dtos.ScheduleResponse{
					ID:           fmt.Sprintf("gomaluum:schedule:%s", cuid.Slug()),
					SessionName:  job.name,
					SessionQuery: job.query,
					Schedule:     subjects,
				},
			}
		}()
	}
}

// Process schedules using worker pool pattern
func (s *Server) processSchedulesWithWorkerPool(ctx context.Context, queries, names []string, cookie string, stale *atomic.Bool) ([]dtos.ScheduleResponse, error) {
	const maxWorkers = 5

	jobs := make(chan scheduleJob, len(queries))
	results := make(chan scheduleResult, len(queries))

	// Start workers
	for range maxWorkers {
		go s.scheduleWorker(ctx, jobs, results, cookie, stale)
	}

	// Send jobs
	go func() {
		defer close(jobs)
		for i := range queries {
			jobs <- scheduleJob{
				query: queries[i],
				name:  names[i],
			}
		}
	}()

	// Collect results
	var schedules []dtos.ScheduleResponse
	var errors []error

	for range queries {
		result := <-results
		if result.err != nil {
			errors = append(errors, result.err)
		} else {
			schedules = append(schedules, result.schedule)
		}
	}

	if len(errors) > 0 {
		return nil, errors[0] // Return first error
	}

	return schedules, nil
}

// latestSessionQuery returns the query of the newest session in the dropdown.
// Past semesters are immutable, so only this one must be re-scraped on a cache
// hit. queries and names are index-aligned.
func latestSessionQuery(queries, names []string) string {
	latest := 0
	for i := 1; i < len(queries) && i < len(names); i++ {
		if utils.SortSessionNames(names[i], names[latest]) {
			latest = i
		}
	}
	return queries[latest]
}

// resolveSchedules returns the schedules for the given sessions, using the GEI
// cache to avoid re-scraping immutable past semesters. When the cache is
// disabled (indexer nil), forced (refresh), or empty, it scrapes everything.
// Otherwise it re-scrapes only the latest session plus any session missing from
// the cache, and serves the rest from cache. It never writes the cache — the
// handler persists the result only after a fully successful (non-stale) scrape.
func (s *Server) resolveSchedules(ctx context.Context, username string, queries, names []string, cookie string, stale *atomic.Bool, refresh bool) ([]dtos.ScheduleResponse, error) {
	if s.indexer == nil || refresh {
		return s.processSchedulesWithWorkerPool(ctx, queries, names, cookie, stale)
	}

	cached, found, err := s.indexer.GetSchedule(ctx, username)
	if err != nil {
		// A cache read failure must not break the request: fall back to a scrape.
		s.log.WarnContext(ctx, "GEI GetSchedule failed, scraping all sessions", "error", err)
		found = false
	}
	if !found || len(cached) == 0 {
		return s.processSchedulesWithWorkerPool(ctx, queries, names, cookie, stale)
	}

	cachedByQuery := make(map[string]dtos.ScheduleResponse, len(cached))
	for _, c := range cached {
		cachedByQuery[c.SessionQuery] = c
	}

	// Re-scrape the latest session (mutable) plus anything not yet cached.
	latest := latestSessionQuery(queries, names)
	var scrapeQueries, scrapeNames []string
	for i, q := range queries {
		if _, ok := cachedByQuery[q]; q == latest || !ok {
			scrapeQueries = append(scrapeQueries, q)
			scrapeNames = append(scrapeNames, names[i])
		}
	}

	var scraped []dtos.ScheduleResponse
	if len(scrapeQueries) > 0 {
		scraped, err = s.processSchedulesWithWorkerPool(ctx, scrapeQueries, scrapeNames, cookie, stale)
		if err != nil {
			return nil, err
		}
	}
	scrapedByQuery := make(map[string]dtos.ScheduleResponse, len(scraped))
	for _, sc := range scraped {
		scrapedByQuery[sc.SessionQuery] = sc
	}

	// Merge over the authoritative dropdown order: fresh scrape wins, else cache.
	// Iterating over queries drops cached sessions no longer offered by i-Ma'luum.
	merged := make([]dtos.ScheduleResponse, 0, len(queries))
	fromCache := 0
	for _, q := range queries {
		if sc, ok := scrapedByQuery[q]; ok {
			merged = append(merged, sc)
		} else if c, ok := cachedByQuery[q]; ok {
			merged = append(merged, c)
			fromCache++
		}
	}

	// Confirms the cached data is actually served: how many sessions came from
	// GEI vs were freshly scraped this request.
	s.log.DebugContext(ctx, "served schedule via GEI cache",
		"username", username,
		"from_cache", fromCache,
		"scraped", len(merged)-fromCache,
		"total", len(merged))
	return merged, nil
}

// @Title ScheduleHandler
// @Description Get schedule from i-Ma'luum
// @Tags scraper
// @Produce json
// @Param x-gomaluum-key header string false "API key for additional security layer"
// @Param Authorization header string true "Insert your access token" default(Bearer <Add access token here>)
// @Success 200 {object} dtos.ResponseDTO
// @Router /api/schedule [get]
func (s *Server) ScheduleHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var (
		logger    = s.log
		cookie    = r.Context().Value(ctxToken).(string)
		schedules []dtos.ScheduleResponse
	)

	// username keys the GEI schedule cache; ?refresh=1 (or ?refresh=true) forces
	// a full re-scrape that bypasses the cache read. Any other value is ignored,
	// so ?refresh=0 keeps using the cache.
	var username string
	if sess, ok := r.Context().Value(ctxSession).(*TokenPayload); ok && sess != nil {
		username = sess.username
	}
	refresh := isTruthyParam(r.URL.Query().Get("refresh"))

	// Return fake data for fake user
	if cookie == constants.DebugUserCookie {
		now := time.Now()
		KLTimezone, err := time.LoadLocation("Asia/Kuala_Lumpur")
		if err != nil {
			logger.ErrorContext(r.Context(), "Error loading timezone", "error", err)
			KLTimezone = time.UTC
		}

		morning9am, _ := time.ParseInLocation("2006-01-02 1504", fmt.Sprintf("%04d-%02d-%02d 0900", now.Year(), now.Month(), now.Day()), KLTimezone)
		morning11am, _ := time.ParseInLocation("2006-01-02 1504", fmt.Sprintf("%04d-%02d-%02d 1100", now.Year(), now.Month(), now.Day()), KLTimezone)
		afternoon2pm, _ := time.ParseInLocation("2006-01-02 1504", fmt.Sprintf("%04d-%02d-%02d 1400", now.Year(), now.Month(), now.Day()), KLTimezone)
		afternoon4pm, _ := time.ParseInLocation("2006-01-02 1504", fmt.Sprintf("%04d-%02d-%02d 1600", now.Year(), now.Month(), now.Day()), KLTimezone)
		evening5pm, _ := time.ParseInLocation("2006-01-02 1504", fmt.Sprintf("%04d-%02d-%02d 1700", now.Year(), now.Month(), now.Day()), KLTimezone)
		evening7pm, _ := time.ParseInLocation("2006-01-02 1504", fmt.Sprintf("%04d-%02d-%02d 1900", now.Year(), now.Month(), now.Day()), KLTimezone)

		fakeSchedule := []dtos.ScheduleResponse{
			{
				ID:           fmt.Sprintf("gomaluum:schedule:%s", cuid.Slug()),
				SessionName:  "2024/2025 Semester 1",
				SessionQuery: "?ses=2024/2025&sem=1",
				Schedule: []dtos.ScheduleSubject{
					{
						ID:         fmt.Sprintf("gomaluum:subject:%s", cuid.Slug()),
						CourseCode: "INFO4335",
						CourseName: "Software Engineering",
						Venue:      "E1-LT4",
						Lecturer:   "Dr. Muhammad Ali bin Ahmad",
						Section:    1,
						Chr:        3.0,
						Timestamps: []dtos.WeekTime{
							{
								Start:     "0900",
								StartUnix: morning9am.Unix(),
								End:       "1100",
								EndUnix:   morning11am.Unix(),
								Day:       1,
							},
							{
								Start:     "0900",
								StartUnix: morning9am.Unix(),
								End:       "1100",
								EndUnix:   morning11am.Unix(),
								Day:       3,
							},
						},
					},
					{
						ID:         fmt.Sprintf("gomaluum:subject:%s", cuid.Slug()),
						CourseCode: "INFO4327",
						CourseName: "Database Systems",
						Venue:      "E2-LT2",
						Lecturer:   "Prof. Dr. Siti Aminah binti Abdullah",
						Section:    2,
						Chr:        3.0,
						Timestamps: []dtos.WeekTime{
							{
								Start:     "1400",
								StartUnix: afternoon2pm.Unix(),
								End:       "1600",
								EndUnix:   afternoon4pm.Unix(),
								Day:       2,
							},
							{
								Start:     "1400",
								StartUnix: afternoon2pm.Unix(),
								End:       "1600",
								EndUnix:   afternoon4pm.Unix(),
								Day:       4,
							},
						},
					},
					{
						ID:         fmt.Sprintf("gomaluum:subject:%s", cuid.Slug()),
						CourseCode: "INFO4501",
						CourseName: "Web Development",
						Venue:      "E3-LAB1",
						Lecturer:   "Dr. Ahmad bin Hassan",
						Section:    1,
						Chr:        4.0,
						Timestamps: []dtos.WeekTime{
							{
								Start:     "0900",
								StartUnix: morning9am.Unix(),
								End:       "1100",
								EndUnix:   morning11am.Unix(),
								Day:       2,
							},
							{
								Start:     "1400",
								StartUnix: afternoon2pm.Unix(),
								End:       "1600",
								EndUnix:   afternoon4pm.Unix(),
								Day:       5,
							},
						},
					},
					{
						ID:         fmt.Sprintf("gomaluum:subject:%s", cuid.Slug()),
						CourseCode: "INFO4210",
						CourseName: "Mobile Application Development",
						Venue:      "E1-LAB2",
						Lecturer:   "Dr. Fatimah binti Ibrahim",
						Section:    3,
						Chr:        3.0,
						Timestamps: []dtos.WeekTime{
							{
								Start:     "1700",
								StartUnix: evening5pm.Unix(),
								End:       "1900",
								EndUnix:   evening7pm.Unix(),
								Day:       3,
							},
						},
					},
					{
						ID:         fmt.Sprintf("gomaluum:subject:%s", cuid.Slug()),
						CourseCode: "UNGS2040",
						CourseName: "Tamadun Islam dan Tamadun Asia (TITAS)",
						Venue:      "KAED-LT1",
						Lecturer:   "Dr. Zainab binti Yusof",
						Section:    5,
						Chr:        2.0,
						Timestamps: []dtos.WeekTime{
							{
								Start:     "0900",
								StartUnix: morning9am.Unix(),
								End:       "1100",
								EndUnix:   morning11am.Unix(),
								Day:       5,
							},
						},
					},
				},
			},
			{
				ID:           fmt.Sprintf("gomaluum:schedule:%s", cuid.Slug()),
				SessionName:  "2023/2024 Semester 2",
				SessionQuery: "?ses=2023/2024&sem=2",
				Schedule: []dtos.ScheduleSubject{
					{
						ID:         fmt.Sprintf("gomaluum:subject:%s", cuid.Slug()),
						CourseCode: "INFO3202",
						CourseName: "Data Structures and Algorithms",
						Venue:      "E2-LT3",
						Lecturer:   "Prof. Dr. Abdul Rahman bin Mohd",
						Section:    1,
						Chr:        3.0,
						Timestamps: []dtos.WeekTime{
							{
								Start:     "1400",
								StartUnix: afternoon2pm.Unix(),
								End:       "1600",
								EndUnix:   afternoon4pm.Unix(),
								Day:       1,
							},
							{
								Start:     "1400",
								StartUnix: afternoon2pm.Unix(),
								End:       "1600",
								EndUnix:   afternoon4pm.Unix(),
								Day:       3,
							},
						},
					},
					{
						ID:         fmt.Sprintf("gomaluum:subject:%s", cuid.Slug()),
						CourseCode: "INFO3150",
						CourseName: "Computer Networks",
						Venue:      "E3-LT1",
						Lecturer:   "Dr. Nurul Huda binti Hassan",
						Section:    2,
						Chr:        3.0,
						Timestamps: []dtos.WeekTime{
							{
								Start:     "0900",
								StartUnix: morning9am.Unix(),
								End:       "1100",
								EndUnix:   morning11am.Unix(),
								Day:       4,
							},
						},
					},
				},
			},
		}

		response := &dtos.ResponseDTO{
			Message: "Successfully fetched schedule",
			Data:    fakeSchedule,
		}

		if err := sonic.ConfigFastest.NewEncoder(w).Encode(response); err != nil {
			logger.ErrorContext(r.Context(), "Failed to encode response", "error", err)
			errors.Render(w, r, errors.ErrFailedToEncodeResponse)
		}
		return
	}

	if err := s.scrapeWithRetry(r.Context(), func(cookie string) (bool, error) {
		var stale atomic.Bool

		// Discover the available sessions from the schedule SPA's data payload: a
		// default page load returns the full session list in all_sem. (i-Ma'luum
		// moved this from a server-rendered dropdown to a JS-populated one.)
		data, err := s.fetchScheduleData(r.Context(), cookie, "", &stale)
		if err != nil {
			return false, err
		}
		if stale.Load() {
			return true, nil
		}
		if data == nil {
			logger.ErrorContext(r.Context(), "No valid sessions found")
			return false, errors.ErrScheduleIsEmpty
		}

		filteredQueries, filteredNames := sessionsFromAllSem(data.AllSem)
		if len(filteredQueries) == 0 {
			logger.ErrorContext(r.Context(), "No valid sessions found")
			return false, errors.ErrScheduleIsEmpty
		}

		result, err := s.resolveSchedules(r.Context(), username, filteredQueries, filteredNames, cookie, &stale, refresh)
		if err != nil {
			return false, err
		}
		if stale.Load() {
			return true, nil
		}
		schedules = result
		return false, nil
	}); err != nil {
		logger.ErrorContext(r.Context(), "Failed to scrape schedule", "error", err)
		errors.Render(w, r, err)
		return
	}

	if len(schedules) == 0 {
		logger.ErrorContext(r.Context(), "Schedule is empty")
		errors.Render(w, r, errors.ErrScheduleIsEmpty)
		return
	}

	// Sort schedules
	sort.Slice(schedules, func(i, j int) bool {
		return utils.SortSessionNames(schedules[i].SessionName, schedules[j].SessionName)
	})

	// Refresh the cache with the successful result (best-effort). Only reached on
	// a non-stale scrape, so the login page can never poison the cache.
	if s.indexer != nil && username != "" {
		if err := s.indexer.StoreSchedule(r.Context(), username, schedules); err != nil {
			logger.WarnContext(r.Context(), "Failed to cache schedule in GEI", "error", err)
		}
	}

	response := &dtos.ResponseDTO{
		Message: "Successfully fetched schedule",
		Data:    schedules,
	}

	if err := sonic.ConfigFastest.NewEncoder(w).Encode(response); err != nil {
		logger.ErrorContext(r.Context(), "Failed to encode response", "error", err)
		errors.Render(w, r, errors.ErrFailedToEncodeResponse)
	}
}
