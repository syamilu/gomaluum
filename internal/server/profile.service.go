package server

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/nrmnqdds/gomaluum/internal/constants"
	"github.com/nrmnqdds/gomaluum/internal/dtos"
	"github.com/nrmnqdds/gomaluum/internal/errors"
)

// Format address efficiently by cleaning and joining lines
func formatAddress(address string) string {
	if address == "" {
		return ""
	}

	lines := strings.Split(address, "\n")
	formattedLines := make([]string, 0, len(lines))

	for _, line := range lines {
		cleaned := strings.TrimSpace(strings.ReplaceAll(line, "\t", " "))
		// Replace multiple spaces with single space
		for strings.Contains(cleaned, "  ") {
			cleaned = strings.ReplaceAll(cleaned, "  ", " ")
		}
		if cleaned != "" {
			formattedLines = append(formattedLines, cleaned)
		}
	}

	return strings.Join(formattedLines, ", ")
}

// Build image URL efficiently
func buildImageURL(matricNo string) string {
	const baseURL = "https://smartcard.iium.edu.my/packages/card/printing/camera/uploads/original/"
	const extension = ".jpeg"

	// Pre-calculate capacity to avoid reallocation
	capacity := len(baseURL) + len(matricNo) + len(extension)
	var builder strings.Builder
	builder.Grow(capacity)

	builder.WriteString(baseURL)
	builder.WriteString(matricNo)
	builder.WriteString(extension)

	return builder.String()
}

func (s *Server) Profile(ctx context.Context, cookie string) (*dtos.Profile, bool, error) {
	logger := s.log

	// Return fake data for fake user
	if cookie == constants.DebugUserCookie {
		return &dtos.Profile{
			Name:          "MUHAMMAD IZZAT BIN ABDUL RAHMAN",
			MatricNo:      "2214227",
			Level:         "4",
			Kuliyyah:      "Kulliyyah of Information and Communication Technology",
			IC:            "010123-01-0456",
			Gender:        "Male",
			Birthday:      "23 Jan 2001",
			Religion:      "Islam",
			MaritalStatus: "Single",
			Address:       "No. 123, Jalan Bunga Raya, Taman Melati, 53100 Kuala Lumpur, Wilayah Persekutuan",
			ImageURL:      "https://smartcard.iium.edu.my/packages/card/printing/camera/uploads/original/2214227.jpeg",
		}, false, nil
	}

	var stale atomic.Bool
	data, err := s.fetchProfileData(ctx, cookie, &stale)
	if err != nil {
		logger.ErrorContext(ctx, "Failed to fetch profile", "error", err)
		return nil, false, err
	}
	if stale.Load() {
		return nil, true, nil
	}
	if data == nil {
		logger.ErrorContext(ctx, "Failed to extract profile data")
		return nil, false, errors.ErrFailedToGoToURL
	}

	return mapImaluumProfile(data), false, nil
}
