package server

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMapImaluumProfile(t *testing.T) {
	t.Run("maps user fields into the public profile shape", func(t *testing.T) {
		data := &imaluumProfileData{
			User: imaluumProfileUser{
				Name:         "MUHAMMAD IZZAT BIN ABDUL RAHMAN",
				MatricNo:     "2214227",
				Level:        "4",
				KulyDesc:     "Kulliyyah of Information and Communication Technology",
				Avatar:       "https://smartcard.iium.edu.my/x/2214227.jpeg",
				IcNo:         "010123-01-0456",
				GenderDesc:   "Male",
				BirthDate:    "23 Jan 2001",
				ReligionDesc: "Islam",
				MaritalDesc:  "Single",
				Address:      "  No. 123,\tJalan  Bunga Raya\n  53100 Kuala Lumpur ",
			},
		}

		p := mapImaluumProfile(data)

		require.Equal(t, "MUHAMMAD IZZAT BIN ABDUL RAHMAN", p.Name)
		require.Equal(t, "2214227", p.MatricNo)
		require.Equal(t, "4", p.Level)
		require.Equal(t, "Kulliyyah of Information and Communication Technology", p.Kuliyyah)
		require.Equal(t, "010123-01-0456", p.IC)
		require.Equal(t, "Male", p.Gender)
		require.Equal(t, "23 Jan 2001", p.Birthday)
		require.Equal(t, "Islam", p.Religion)
		require.Equal(t, "Single", p.MaritalStatus)
		// Address is cleaned + joined the same way the old scraper formatted it.
		require.Equal(t, "No. 123, Jalan Bunga Raya, 53100 Kuala Lumpur", p.Address)
		// The payload's avatar URL is authoritative when present.
		require.Equal(t, "https://smartcard.iium.edu.my/x/2214227.jpeg", p.ImageURL)
	})

	t.Run("falls back to the smartcard URL when avatar is empty", func(t *testing.T) {
		p := mapImaluumProfile(&imaluumProfileData{
			User: imaluumProfileUser{MatricNo: "2214227"},
		})
		require.Equal(t, buildImageURL("2214227"), p.ImageURL)
	})

	t.Run("falls back to the smartcard URL when avatar is a base64 data URI", func(t *testing.T) {
		// i-Ma'luum's SPA embeds the avatar as a data: URI. image_url must stay a
		// real URL (the app loads it over the network), so fall back to smartcard.
		p := mapImaluumProfile(&imaluumProfileData{
			User: imaluumProfileUser{MatricNo: "2214227", Avatar: "data:image/jpeg;base64,/9j/4AAQSkZ"},
		})
		require.Equal(t, buildImageURL("2214227"), p.ImageURL)
	})

	t.Run("keeps a real http avatar URL when the payload provides one", func(t *testing.T) {
		p := mapImaluumProfile(&imaluumProfileData{
			User: imaluumProfileUser{MatricNo: "2214227", Avatar: "https://cdn.iium.edu.my/a.jpg"},
		})
		require.Equal(t, "https://cdn.iium.edu.my/a.jpg", p.ImageURL)
	})
}

// Guard: the raw DTO must decode the real /Profile data JSON shape (BOM-stripped),
// ignoring the network/vehicles/vaccine sections the public profile does not use.
func TestDecodeProfileData(t *testing.T) {
	body := "\ufeff" + `{"data":{"user":{"name":"IZZAT","matric_no":"2214227","level":"4","kuly_desc":"KICT","ic_no":"01","gender_desc":"Male","birth_date":"23 Jan 2001","religion_desc":"Islam","marital_desc":"Single","address":"KL","avatar":"http://x/a.jpg"},"network":{"in_range":false},"vehicles":[],"vaccine":[]}}`

	data, err := decodeProfileData([]byte(body))
	require.NoError(t, err)
	require.NotNil(t, data)
	require.Equal(t, "IZZAT", data.User.Name)
	require.Equal(t, "2214227", data.User.MatricNo)
	require.Equal(t, "http://x/a.jpg", data.User.Avatar)
}

func TestDecodeProfileDataNull(t *testing.T) {
	data, err := decodeProfileData([]byte(`{"data":null}`))
	require.NoError(t, err)
	require.Nil(t, data)
}
