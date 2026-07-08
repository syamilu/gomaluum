package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/bytedance/sonic"
	"github.com/gocolly/colly/v2"
	"github.com/nrmnqdds/gomaluum/internal/constants"
	"github.com/nrmnqdds/gomaluum/internal/dtos"
	"github.com/nrmnqdds/gomaluum/internal/errors"
)

// i-Ma'luum migrated /Profile from server-rendered HTML to a JavaScript SPA, the
// same way it did /MyAcademic/schedule. The page ships a <div id="profilenew-app"
// data-endpoint="..." data-token="..."> and the browser fetches the profile JSON
// from that endpoint with an "X-Page-Token" header. A plain scrape of the page
// therefore reads only "Loading parameters..." placeholders, so these helpers
// reproduce the SPA's data fetch instead.

// imaluumProfileResponse is the envelope returned by the profile data endpoint.
// Data is nil when i-Ma'luum has no profile to show. Only the "user" section is
// mapped; the network/vehicles/vaccine sections are unused by the public profile.
type imaluumProfileResponse struct {
	Data *imaluumProfileData `json:"data"`
}

type imaluumProfileData struct {
	User imaluumProfileUser `json:"user"`
}

type imaluumProfileUser struct {
	Name         string `json:"name"`
	MatricNo     string `json:"matric_no"`
	Level        string `json:"level"`
	KulyDesc     string `json:"kuly_desc"`
	Avatar       string `json:"avatar"`
	IcNo         string `json:"ic_no"`
	GenderDesc   string `json:"gender_desc"`
	BirthDate    string `json:"birth_date"`
	ReligionDesc string `json:"religion_desc"`
	MaritalDesc  string `json:"marital_desc"`
	Address      string `json:"address"`
}

// mapImaluumProfile converts the raw SPA payload into the public Profile shape.
// The payload's avatar URL is authoritative; when absent it falls back to the
// smartcard URL derived from the matric number, matching the old scraper.
func mapImaluumProfile(data *imaluumProfileData) *dtos.Profile {
	u := data.User

	imageURL := u.Avatar
	if imageURL == "" {
		imageURL = buildImageURL(u.MatricNo)
	}

	return &dtos.Profile{
		Name:          u.Name,
		MatricNo:      u.MatricNo,
		Level:         u.Level,
		Kuliyyah:      u.KulyDesc,
		IC:            u.IcNo,
		Gender:        u.GenderDesc,
		Birthday:      u.BirthDate,
		Religion:      u.ReligionDesc,
		MaritalStatus: u.MaritalDesc,
		Address:       formatAddress(u.Address),
		ImageURL:      imageURL,
	}
}

// decodeProfileData unmarshals a profile data-endpoint response body, stripping
// the leading UTF-8 BOM i-Ma'luum prepends. Returns a nil *data (no error) when
// the payload's "data" field is null.
func decodeProfileData(body []byte) (*imaluumProfileData, error) {
	body = bytes.TrimPrefix(body, []byte("\ufeff"))
	var resp imaluumProfileResponse
	if err := sonic.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// fetchProfileData reproduces the profile SPA's data fetch: load /Profile to read
// the data endpoint and a fresh single-use X-Page-Token from #profilenew-app,
// then GET that endpoint with the token. Returns a nil *data (no error) when the
// profile has no records. A nil return with stale set means the session cookie
// expired; the caller retries.
func (s *Server) fetchProfileData(ctx context.Context, cookie string, stale *atomic.Bool) (*imaluumProfileData, error) {
	var endpoint, token string
	c := s.newImaluumCollector(ctx, cookie, stale)
	c.OnHTML("#profilenew-app", func(e *colly.HTMLElement) {
		endpoint = e.Attr("data-endpoint")
		token = e.Attr("data-token")
	})
	if err := c.Visit(constants.ImaluumProfilePage); err != nil {
		return nil, classifyVisitError(err)
	}
	if stale.Load() {
		return nil, nil
	}
	if endpoint == "" || token == "" {
		// Page loaded but no mount data: the SPA markup changed. Surface it rather
		// than silently returning an empty profile.
		return nil, errors.ErrFailedToGoToURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
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
		return nil, errors.Wrap(errors.ErrFailedToGoToURL, fmt.Errorf("profile data endpoint returned %d", resp.StatusCode))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(errors.ErrFailedToGoToURL, err)
	}
	return decodeProfileData(body)
}
