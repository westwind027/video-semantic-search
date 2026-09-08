package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type Resolution struct {
	Title string `json:"title"`
	Year  *int   `json:"year,omitempty"`
}

// EntityID gives metadata entities stable local IDs without relying on a
// database sequence. External IDs win; title/year is a deterministic fallback
// for manually imported metadata.
func EntityID(kind string, tmdbID int, externalID, title string, year *int) string {
	kind = strings.TrimSpace(kind)
	if tmdbID > 0 {
		return fmt.Sprintf("%s:tmdb:%d", kind, tmdbID)
	}
	if externalID = strings.TrimSpace(externalID); externalID != "" {
		return kind + ":external:" + strings.ToLower(externalID)
	}
	value := kind + ":" + strings.ToLower(strings.TrimSpace(title))
	if year != nil {
		value += fmt.Sprintf(":%d", *year)
	}
	digest := sha256.Sum256([]byte(value))
	return kind + ":local:" + hex.EncodeToString(digest[:8])
}

var yearPattern = regexp.MustCompile(`(?:^|[ ._\-\[\(])((?:18|19|20|21)\d{2})(?:$|[ ._\-\]\)])`)

// ResolveFilename extracts a conservative title/year pair from a media file
// name. It intentionally stops at the year and strips release punctuation;
// it is a resolver hint, not an IMDb scraper.
func ResolveFilename(name string) Resolution {
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	base = strings.TrimSpace(base)
	result := Resolution{Title: cleanTitle(base)}
	if match := yearPattern.FindStringSubmatchIndex(base); match != nil {
		year, err := strconv.Atoi(base[match[2]:match[3]])
		if err == nil {
			result.Year = &year
			prefix := strings.TrimSpace(base[:match[0]])
			result.Title = cleanTitle(prefix)
		}
	}
	return result
}

func cleanTitle(value string) string {
	value = strings.ReplaceAll(value, "_", " ")
	value = strings.ReplaceAll(value, ".", " ")
	value = strings.ReplaceAll(value, "-", " ")
	value = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Han, r) {
			return r
		}
		return ' '
	}, value)
	return strings.Join(strings.Fields(value), " ")
}
