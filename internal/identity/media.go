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
		if movie, ok := catalog.FindMovie(hint.title, hint.year); ok && movieCanonicalTitleMatches(movie, hint.title) {
			return movie, true
		}
	}

	searcher, ok := catalog.(MovieSearcher)
	if !ok {
		return model.Movie{}, false
	}
	titleSearcher, hasFastTitleSearch := catalog.(MovieTitleSearcher)
	exactTitleSearcher, hasExactTitleSearch := catalog.(MovieExactTitleSearcher)
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
	rankCandidates := func(searchMode int) (model.Movie, int) {
		var best model.Movie
		bestScore := -1
		for _, query := range queries {
			candidates := []model.Movie(nil)
			switch {
			case searchMode == 0 && hasFastTitleSearch:
				candidates = titleSearcher.SearchMoviesByTitle(query, 100)
			case searchMode == 1 && hasExactTitleSearch:
				candidates = exactTitleSearcher.SearchMoviesByExactTitle(query, 100)
			default:
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
		return best, bestScore
	}
	best, bestScore := rankCandidates(0)
	// The SQLite title-only path intentionally skips the large alias table.
	// If it only found a partial release-title match, or an alias with the
	// wrong year, retry through the full search so IMDb alternate titles can
	// resolve names such as "Amelie" -> canonical "Amélie".
	if bestScore >= 0 && (expectedYear != nil || movieCanonicalTitleMatchesAny(best, hints)) {
		return best, true
	}
	best, bestScore = rankCandidates(1)
	if bestScore < 0 && hasExactTitleSearch {
		// Exact lookup is the normal path. Retain a broad fallback for unusual
		// release names whose title token is not an exact IMDb alias.
		best, bestScore = rankCandidates(2)
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
		value = normalizeLoose(value)
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
			value = normalizeLoose(value)
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
	query = normalizeLoose(query)
	score := 0
	if expectedYear != nil && movie.Year != nil && *expectedYear == *movie.Year {
		score += 1000
	}
	for _, value := range []string{movie.Title, movie.OriginalTitle} {
		value = normalizeLoose(value)
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
		for _, token := range strings.Fields(normalizeLoose(hint.title)) {
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

func movieCanonicalTitleMatches(movie model.Movie, title string) bool {
	want := normalizeLoose(title)
	return want != "" && (normalizeLoose(movie.Title) == want || normalizeLoose(movie.OriginalTitle) == want)
}

func movieCanonicalTitleMatchesAny(movie model.Movie, hints []movieHint) bool {
	for _, hint := range hints {
		if movieCanonicalTitleMatches(movie, hint.title) {
			return true
		}
	}
	return false
}

var movieDiacriticReplacer = strings.NewReplacer(
	"À", "A", "Á", "A", "Â", "A", "Ã", "A", "Ä", "A", "Å", "A",
	"à", "a", "á", "a", "â", "a", "ã", "a", "ä", "a", "å", "a",
	"Æ", "AE", "æ", "ae", "Ç", "C", "ç", "c", "Ð", "D", "ð", "d",
	"È", "E", "É", "E", "Ê", "E", "Ë", "E", "è", "e", "é", "e", "ê", "e", "ë", "e",
	"Ì", "I", "Í", "I", "Î", "I", "Ï", "I", "ì", "i", "í", "i", "î", "i", "ï", "i",
	"Ñ", "N", "ñ", "n", "Ò", "O", "Ó", "O", "Ô", "O", "Õ", "O", "Ö", "O",
	"ò", "o", "ó", "o", "ô", "o", "õ", "o", "ö", "o", "Ø", "O", "ø", "o",
	"Œ", "OE", "œ", "oe", "Ù", "U", "Ú", "U", "Û", "U", "Ü", "U",
	"ù", "u", "ú", "u", "û", "u", "ü", "u", "Ý", "Y", "Ÿ", "Y", "ý", "y", "ÿ", "y",
	"Š", "S", "š", "s", "Ž", "Z", "ž", "z", "Ł", "L", "ł", "l", "ß", "ss",
)

func normalizeLoose(value string) string {
	return normalize(movieDiacriticReplacer.Replace(value))
}

func metadataString(values map[string]any, key string) string {
	if value, ok := values[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}
