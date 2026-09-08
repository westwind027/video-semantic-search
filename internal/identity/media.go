package identity

import (
	"sort"
	"strings"
	"unicode"

	"video-semantic-search/internal/metadata"
	"video-semantic-search/internal/model"
)

// ResolveMovieForMedia associates an indexed media item with the local IMDb
// movie catalog. New acquisitions carry MovieID directly. Older index entries
// may only have a release filename, so the fallback uses the parsed year and a
// distinctive title token without scanning the complete catalog.
func ResolveMovieForMedia(catalog Store, media model.Media) (model.Movie, bool) {
	if catalog == nil {
		return model.Movie{}, false
	}
	for _, id := range []string{media.MovieID, metadataString(media.Metadata, "movie_id")} {
		if id == "" {
			continue
		}
		if movie, ok := catalog.GetMovie(id); ok {
			return movie, true
		}
	}

	hints := mediaMovieHints(media)
	for _, hint := range hints {
		if strings.TrimSpace(hint.title) == "" {
			continue
		}
		// Release names contain punctuation, codec tags and often a language
		// prefix. Exact lookup is useful for a clean title, but an exact
		// alternate-title subquery is expensive on the full IMDb alias table.
		if strings.ContainsAny(hint.title, "._[]()") || len(strings.Fields(hint.title)) > 12 {
			continue
		}
		if movie, ok := catalog.FindMovie(hint.title, hint.year); ok {
			return movie, true
		}
	}

	searcher, ok := catalog.(MovieSearcher)
	if !ok {
		return model.Movie{}, false
	}
	titleSearcher, hasFastTitleSearch := catalog.(MovieTitleSearcher)
	queries := movieSearchQueries(hints)
	expectedYear := media.Year
	if expectedYear == nil {
		for _, hint := range hints {
			if hint.year != nil {
				expectedYear = hint.year
				break
			}
		}
	}
	var best model.Movie
	bestScore := -1
	for _, query := range queries {
		candidates := []model.Movie(nil)
		if hasFastTitleSearch {
			candidates = titleSearcher.SearchMoviesByTitle(query, 100)
		} else {
			candidates = searcher.SearchMovies(query, 100)
		}
		for _, movie := range candidates {
			if expectedYear != nil && (movie.Year == nil || *expectedYear != *movie.Year) {
				continue
			}
			score := movieHintScore(movie, hints, query, expectedYear)
			if score > bestScore || (score == bestScore && movie.ID < best.ID) {
				best = movie
				bestScore = score
			}
		}
	}
	return best, bestScore >= 0
}

type movieHint struct {
	title string
	year  *int
}

func mediaMovieHints(media model.Media) []movieHint {
	values := []string{media.Title, media.OriginalTitle, metadataString(media.Metadata, "remote_name")}
	hints := make([]movieHint, 0, len(values)*2)
	seen := make(map[string]struct{})
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		addMovieHint := func(title string, year *int) {
			key := normalize(title)
			if key == "" {
				return
			}
			if _, ok := seen[key]; ok {
				return
			}
			seen[key] = struct{}{}
			hints = append(hints, movieHint{title: title, year: year})
		}
		addMovieHint(value, media.Year)
		resolved := metadata.ResolveFilename(value)
		addMovieHint(resolved.Title, resolved.Year)
	}
	return hints
}

func movieSearchQueries(hints []movieHint) []string {
	type token struct {
		value string
		len   int
	}
	seen := make(map[string]struct{})
	queries := make([]string, 0, len(hints)*3)
	add := func(value string) {
		value = normalize(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		queries = append(queries, value)
	}
	tokens := make([]token, 0)
	for _, hint := range hints {
		for _, value := range strings.Fields(hint.title) {
			value = normalize(value)
			if len([]rune(value)) < 4 || !usefulMovieToken(value) {
				continue
			}
			tokens = append(tokens, token{value: value, len: len([]rune(value))})
		}
	}
	sort.SliceStable(tokens, func(left, right int) bool {
		if tokens[left].len != tokens[right].len {
			return tokens[left].len > tokens[right].len
		}
		return tokens[left].value < tokens[right].value
	})
	for _, item := range tokens {
		add(item.value)
	}
	for _, hint := range hints {
		add(hint.title)
	}
	return queries
}

func usefulMovieToken(value string) bool {
	for _, char := range value {
		if char > unicode.MaxASCII || !unicode.IsLetter(char) {
			return false
		}
	}
	switch value {
	case "about", "after", "before", "from", "into", "movie", "the", "this", "with":
		return false
	default:
		return true
	}
}

func movieHintScore(movie model.Movie, hints []movieHint, query string, expectedYear *int) int {
	score := 0
	if expectedYear != nil && movie.Year != nil && *expectedYear == *movie.Year {
		score += 1000
	}
	for _, value := range []string{movie.Title, movie.OriginalTitle} {
		value = normalize(value)
		if value == "" {
			continue
		}
		if value == query {
			score += 500
		}
		if strings.HasPrefix(value, query) {
			score += 10
		}
		if strings.Contains(value, query) {
			score += len([]rune(query))
		}
	}
	for _, hint := range hints {
		for _, token := range strings.Fields(normalize(hint.title)) {
			if !usefulMovieToken(token) {
				continue
			}
			for _, value := range []string{movie.Title, movie.OriginalTitle} {
				if strings.Contains(normalize(value), token) {
					// Full release-title token overlap is more reliable than the
					// individual SearchMovies query. This prevents a generic
					// token such as "pursuit" from outranking a title that also
					// contains the distinctive "happyness" token.
					score += 100 + len([]rune(token))
					break
				}
			}
		}
	}
	return score
}

func metadataString(values map[string]any, key string) string {
	if value, ok := values[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}
