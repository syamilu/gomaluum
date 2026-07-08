package server

import (
	"testing"

	"github.com/nrmnqdds/gomaluum/internal/dtos"
	"github.com/stretchr/testify/require"
)

func TestBuildSessionQuery(t *testing.T) {
	require.Equal(t, "?ses=2024/2025&sem=2", buildSessionQuery("2024/2025", "2"))
}

func TestBuildSessionName(t *testing.T) {
	// Must match the format utils.SortSessionNames expects: "Sem <n>, <year>".
	require.Equal(t, "Sem 2, 2024/2025", buildSessionName("2", "2024/2025"))
}

func TestMapImaluumSchedule(t *testing.T) {
	t.Run("maps a subject with a two-day slot into per-day timestamps", func(t *testing.T) {
		data := &imaluumScheduleData{
			Ses: "2025/2026",
			Sem: "2",
			Subjects: []imaluumRawSubject{
				{
					Code:  "CSCI 4312",
					Sect:  "1",
					Stat:  "Registered",
					Title: "BLOCKCHAIN AND APPLICATION",
					Chr:   "3",
					Schedule: []imaluumRawSlot{
						{
							Day:   "T-TH",
							Start: "1400",
							Ends:  "1520",
							Lect:  "NORZARIYAH BINTI YAHYA",
							Venue: "ICT TL-E5-01 LEVEL 5E",
						},
					},
				},
			},
		}

		subjects := mapImaluumSchedule(data)

		require.Len(t, subjects, 1)
		s := subjects[0]
		require.Equal(t, "CSCI 4312", s.CourseCode)
		require.Equal(t, "BLOCKCHAIN AND APPLICATION", s.CourseName)
		require.Equal(t, "ICT TL-E5-01 LEVEL 5E", s.Venue)
		require.Equal(t, "NORZARIYAH BINTI YAHYA", s.Lecturer)
		require.Equal(t, 3.0, s.Chr)
		require.Equal(t, uint32(1), s.Section)
		require.NotEmpty(t, s.ID)

		// "T-TH" is Tuesday (2) and Thursday (4) at the same time.
		require.Len(t, s.Timestamps, 2)
		require.Equal(t, uint8(2), s.Timestamps[0].Day)
		require.Equal(t, uint8(4), s.Timestamps[1].Day)
		require.Equal(t, "1400", s.Timestamps[0].Start)
		require.Equal(t, "1520", s.Timestamps[0].End)
		require.NotZero(t, s.Timestamps[0].StartUnix)
		require.NotZero(t, s.Timestamps[0].EndUnix)
	})

	t.Run("returns empty slice for data with no subjects", func(t *testing.T) {
		require.Empty(t, mapImaluumSchedule(&imaluumScheduleData{Ses: "2025/2026", Sem: "2"}))
	})

	t.Run("tolerates non-numeric section and credit hours", func(t *testing.T) {
		data := &imaluumScheduleData{
			Subjects: []imaluumRawSubject{
				{Code: "X", Sect: "", Chr: "", Schedule: []imaluumRawSlot{}},
			},
		}
		subjects := mapImaluumSchedule(data)
		require.Len(t, subjects, 1)
		require.Equal(t, uint32(0), subjects[0].Section)
		require.Equal(t, 0.0, subjects[0].Chr)
		require.Empty(t, subjects[0].Timestamps)
	})
}

func TestSessionsFromAllSem(t *testing.T) {
	t.Run("builds index-aligned queries and names, filtering placeholders", func(t *testing.T) {
		all := []imaluumSessionRef{
			{Ses: "2025/2026", Sem: "2"},
			{Ses: "2024/2025", Sem: "1"},
			{Ses: "0000/0000", Sem: "0"}, // placeholder, must be dropped
		}

		queries, names := sessionsFromAllSem(all)

		require.Equal(t, []string{"?ses=2025/2026&sem=2", "?ses=2024/2025&sem=1"}, queries)
		require.Equal(t, []string{"Sem 2, 2025/2026", "Sem 1, 2024/2025"}, names)
	})

	t.Run("empty input yields empty slices", func(t *testing.T) {
		queries, names := sessionsFromAllSem(nil)
		require.Empty(t, queries)
		require.Empty(t, names)
	})
}

// Guard: the raw DTO must decode the real i-Ma'luum JSON shape (BOM-stripped).
func TestDecodeImaluumScheduleResponse(t *testing.T) {
	body := "\ufeff" + `{"data":{"matric_no":"0000000","ses":"2025/2026","sem":"2","subjects":[{"code":"CSCI 4312","sect":"1","stat":"Registered","title":"BLOCKCHAIN","chr":"3","schedule":[{"day":"T-TH","start":"1400","ends":"1520","lect":"L","venue":"V"}]}],"all_sem":[{"ses":"2025/2026","sem":"2"}]}}`

	data, err := decodeScheduleData([]byte(body))
	require.NoError(t, err)
	require.NotNil(t, data)
	require.Equal(t, "2025/2026", data.Ses)
	require.Len(t, data.Subjects, 1)
	require.Len(t, data.AllSem, 1)

	var _ dtos.ScheduleSubject // ensure dtos import stays used
}

func TestDecodeImaluumScheduleResponseNullData(t *testing.T) {
	data, err := decodeScheduleData([]byte(`{"data":null}`))
	require.NoError(t, err)
	require.Nil(t, data)
}
