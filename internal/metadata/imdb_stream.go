package metadata

import (
	"context"
	"fmt"
	"strings"
)

// IMDbStreamHandlers is the small interface between the TSV parser and a
// durable catalog adapter. Handlers are called in dataset order and never
// retain parser-owned buffers, so a caller can import the complete IMDb
// snapshot without materialising it in memory.
type IMDbStreamHandlers struct {
	Title          func(IMDbTitle) error
	Rating         func(titleID string, averageRating float32, voteCount int64) error
	AlternateTitle func(IMDbAlternateTitle) error
	Crew           func(titleID string, directors, writers []string) error
	Principal      func(IMDbPrincipal) error
	// NameFilter lets a streaming sink discard the usually much larger
	// name.basics dataset before it reaches a database statement. The filter is
	// evaluated after parsing the nconst and before invoking Name.
	NameFilter func(nameID string) bool
	Name       func(IMDbName) error
}

// IMDbAlternateTitle retains the identity fields from title.akas that are
// useful when resolving a local filename. The catalog sink can keep multiple
// translations of the same title because region/language are part of the key.
type IMDbAlternateTitle struct {
	TitleID    string
	Ordering   int
	Title      string
	Region     string
	Language   string
	IsOriginal bool
}

// StreamIMDb streams all supported IMDb datasets to handlers. The basics
// file applies the title selection options; later files are intentionally
// streamed without building a selected-ID map because a database sink can
// discard rows with one indexed lookup. This is what keeps a full import
// bounded by the database batch size rather than the size of IMDb.
func StreamIMDb(ctx context.Context, paths IMDbDatasetPaths, options IMDbImportOptions, handlers IMDbStreamHandlers) error {
	if strings.TrimSpace(paths.Basics) == "" {
		return fmt.Errorf("IMDb title.basics path is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	titlesRead := 0
	if handlers.Title != nil {
		if err := scanTSV(ctx, paths.Basics, func(fields []string) error {
			if len(fields) < 9 || fields[0] == "tconst" || fields[1] != "movie" || !selectedTitle(fields[0], options.TitleIDs) {
				return nil
			}
			adult := fields[4] == "1"
			if adult && !options.IncludeAdult {
				return nil
			}
			startYear := parseYear(fields[5])
			if options.MinYear > 0 && (startYear == nil || *startYear < options.MinYear) {
				return nil
			}
			if options.MaxYear > 0 && (startYear == nil || *startYear > options.MaxYear) {
				return nil
			}
			if options.MaxMovies > 0 && titlesRead >= options.MaxMovies {
				return nil
			}
			titlesRead++
			return handlers.Title(IMDbTitle{
				ID:             fields[0],
				TitleType:      fields[1],
				PrimaryTitle:   unescape(fields[2]),
				OriginalTitle:  unescape(fields[3]),
				IsAdult:        adult,
				StartYear:      startYear,
				EndYear:        parseYear(fields[6]),
				RuntimeMinutes: parseInt(fields[7]),
				Genres:         splitList(fields[8]),
			})
		}); err != nil {
			return err
		}
	}

	if handlers.Rating != nil && paths.Ratings != "" {
		if err := scanTSV(ctx, paths.Ratings, func(fields []string) error {
			if len(fields) < 3 || fields[0] == "tconst" {
				return nil
			}
			return handlers.Rating(fields[0], parseFloat32(fields[1]), parseInt64(fields[2]))
		}); err != nil {
			return err
		}
	}

	if handlers.AlternateTitle != nil && paths.Akas != "" {
		if err := scanTSV(ctx, paths.Akas, func(fields []string) error {
			if len(fields) < 3 || fields[0] == "titleId" {
				return nil
			}
			title := strings.TrimSpace(unescape(fields[2]))
			if title == "" {
				return nil
			}
			alias := IMDbAlternateTitle{TitleID: fields[0], Ordering: parseInt(fields[1]), Title: title}
			if len(fields) > 3 {
				alias.Region = unescape(fields[3])
			}
			if len(fields) > 4 {
				alias.Language = unescape(fields[4])
			}
			if len(fields) > 7 {
				alias.IsOriginal = fields[7] == "1"
			}
			return handlers.AlternateTitle(alias)
		}); err != nil {
			return err
		}
	}

	if handlers.Crew != nil && paths.Crew != "" {
		if err := scanTSV(ctx, paths.Crew, func(fields []string) error {
			if len(fields) < 3 || fields[0] == "tconst" {
				return nil
			}
			return handlers.Crew(fields[0], splitList(fields[1]), splitList(fields[2]))
		}); err != nil {
			return err
		}
	}

	if handlers.Principal != nil && paths.Principals != "" {
		if err := scanTSV(ctx, paths.Principals, func(fields []string) error {
			if len(fields) < 6 || fields[0] == "tconst" || (fields[3] != "actor" && fields[3] != "actress") {
				return nil
			}
			return handlers.Principal(IMDbPrincipal{
				TitleID:    fields[0],
				Ordering:   parseInt(fields[1]),
				NameID:     fields[2],
				Category:   fields[3],
				Characters: unescape(fields[5]),
			})
		}); err != nil {
			return err
		}
	}

	if handlers.Name != nil && paths.Names != "" {
		if err := scanTSV(ctx, paths.Names, func(fields []string) error {
			if len(fields) < 6 || fields[0] == "nconst" {
				return nil
			}
			if handlers.NameFilter != nil && !handlers.NameFilter(fields[0]) {
				return nil
			}
			return handlers.Name(IMDbName{
				ID:                 fields[0],
				PrimaryName:        unescape(fields[1]),
				BirthYear:          parseYear(fields[2]),
				DeathYear:          parseYear(fields[3]),
				PrimaryProfessions: splitList(fields[4]),
				KnownForTitles:     splitList(fields[5]),
			})
		}); err != nil {
			return err
		}
	}
	return nil
}
