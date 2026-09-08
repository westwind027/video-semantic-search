package metadata

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// IMDbDatasetPaths points at the official IMDb TSV snapshots. All files may
// be either .tsv or .tsv.gz; the importer only opens files that are supplied.
type IMDbDatasetPaths struct {
	Basics     string
	Ratings    string
	Principals string
	Names      string
	Akas       string
	Crew       string
}

type IMDbImportOptions struct {
	MinYear      int
	MaxYear      int
	MaxMovies    int
	IncludeAdult bool
	TitleIDs     map[string]struct{}
}

type IMDbTitle struct {
	ID              string
	TitleType       string
	PrimaryTitle    string
	OriginalTitle   string
	IsAdult         bool
	StartYear       *int
	EndYear         *int
	RuntimeMinutes  int
	Genres          []string
	AlternateTitles []string
	Directors       []string
	Writers         []string
	AverageRating   float32
	VoteCount       int64
}

type IMDbPrincipal struct {
	TitleID    string
	Ordering   int
	NameID     string
	Category   string
	Characters string
}

type IMDbName struct {
	ID                 string
	PrimaryName        string
	BirthYear          *int
	DeathYear          *int
	PrimaryProfessions []string
	KnownForTitles     []string
}

type IMDbCatalog struct {
	Titles     []IMDbTitle
	Principals map[string][]IMDbPrincipal
	Names      map[string]IMDbName
}

// LoadCatalog reads only movie rows selected by the options, then joins the
// optional ratings, principals and names files for those movies. This keeps a
// local-library import bounded instead of loading the entire IMDb universe by
// default.
func LoadCatalog(ctx context.Context, paths IMDbDatasetPaths, options IMDbImportOptions) (IMDbCatalog, error) {
	if strings.TrimSpace(paths.Basics) == "" {
		return IMDbCatalog{}, fmt.Errorf("IMDb title.basics path is required")
	}
	titles := make([]IMDbTitle, 0)
	selected := make(map[string]struct{})
	titleIndex := make(map[string]int)
	err := scanTSV(ctx, paths.Basics, func(fields []string) error {
		if len(fields) < 9 || fields[0] == "tconst" {
			return nil
		}
		if fields[1] != "movie" || !selectedTitle(fields[0], options.TitleIDs) {
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
		if options.MaxMovies > 0 && len(titles) >= options.MaxMovies {
			return nil
		}
		endYear := parseYear(fields[6])
		title := IMDbTitle{ID: fields[0], TitleType: fields[1], PrimaryTitle: unescape(fields[2]), OriginalTitle: unescape(fields[3]), IsAdult: adult, StartYear: startYear, EndYear: endYear, RuntimeMinutes: parseInt(fields[7]), Genres: splitList(fields[8])}
		titles = append(titles, title)
		selected[title.ID] = struct{}{}
		titleIndex[title.ID] = len(titles) - 1
		return nil
	})
	if err != nil {
		return IMDbCatalog{}, err
	}
	if len(titles) == 0 {
		return IMDbCatalog{Titles: titles, Principals: map[string][]IMDbPrincipal{}, Names: map[string]IMDbName{}}, nil
	}
	catalog := IMDbCatalog{Titles: titles, Principals: make(map[string][]IMDbPrincipal), Names: make(map[string]IMDbName)}
	if paths.Ratings != "" {
		if err := scanTSV(ctx, paths.Ratings, func(fields []string) error {
			if len(fields) < 3 || fields[0] == "tconst" {
				return nil
			}
			if _, ok := selected[fields[0]]; !ok {
				return nil
			}
			if index, ok := titleIndex[fields[0]]; ok {
				catalog.Titles[index].AverageRating = parseFloat32(fields[1])
				catalog.Titles[index].VoteCount = parseInt64(fields[2])
			}
			return nil
		}); err != nil {
			return IMDbCatalog{}, err
		}
	}
	if paths.Akas != "" {
		if err := scanTSV(ctx, paths.Akas, func(fields []string) error {
			if len(fields) < 3 || fields[0] == "titleId" {
				return nil
			}
			index, ok := titleIndex[fields[0]]
			if !ok || strings.TrimSpace(unescape(fields[2])) == "" {
				return nil
			}
			catalog.Titles[index].AlternateTitles = appendUnique(catalog.Titles[index].AlternateTitles, unescape(fields[2]))
			return nil
		}); err != nil {
			return IMDbCatalog{}, err
		}
	}
	if paths.Crew != "" {
		if err := scanTSV(ctx, paths.Crew, func(fields []string) error {
			if len(fields) < 3 || fields[0] == "tconst" {
				return nil
			}
			index, ok := titleIndex[fields[0]]
			if !ok {
				return nil
			}
			catalog.Titles[index].Directors = splitList(fields[1])
			catalog.Titles[index].Writers = splitList(fields[2])
			return nil
		}); err != nil {
			return IMDbCatalog{}, err
		}
	}
	nameIDs := make(map[string]struct{})
	if paths.Principals != "" {
		if err := scanTSV(ctx, paths.Principals, func(fields []string) error {
			if len(fields) < 6 || fields[0] == "tconst" {
				return nil
			}
			if _, ok := selected[fields[0]]; !ok || (fields[3] != "actor" && fields[3] != "actress") {
				return nil
			}
			principal := IMDbPrincipal{TitleID: fields[0], Ordering: parseInt(fields[1]), NameID: fields[2], Category: fields[3], Characters: unescape(fields[5])}
			catalog.Principals[principal.TitleID] = append(catalog.Principals[principal.TitleID], principal)
			nameIDs[principal.NameID] = struct{}{}
			return nil
		}); err != nil {
			return IMDbCatalog{}, err
		}
	}
	if paths.Names != "" && len(nameIDs) > 0 {
		if err := scanTSV(ctx, paths.Names, func(fields []string) error {
			if len(fields) < 6 || fields[0] == "nconst" {
				return nil
			}
			if _, ok := nameIDs[fields[0]]; !ok {
				return nil
			}
			catalog.Names[fields[0]] = IMDbName{ID: fields[0], PrimaryName: unescape(fields[1]), BirthYear: parseYear(fields[2]), DeathYear: parseYear(fields[3]), PrimaryProfessions: splitList(fields[4]), KnownForTitles: splitList(fields[5])}
			return nil
		}); err != nil {
			return IMDbCatalog{}, err
		}
	}
	return catalog, nil
}

func selectedTitle(id string, ids map[string]struct{}) bool {
	if len(ids) == 0 {
		return true
	}
	_, ok := ids[id]
	return ok
}

func scanTSV(ctx context.Context, path string, visit func([]string) error) error {
	reader, closeReader, err := openDataset(path)
	if err != nil {
		return err
	}
	defer closeReader()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := visit(strings.Split(scanner.Text(), "\t")); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func openDataset(path string) (io.Reader, func() error, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open IMDb dataset %q: %w", path, err)
	}
	if strings.HasSuffix(strings.ToLower(path), ".gz") {
		compressed, gzipErr := gzip.NewReader(file)
		if gzipErr != nil {
			_ = file.Close()
			return nil, nil, fmt.Errorf("open IMDb gzip dataset %q: %w", path, gzipErr)
		}
		return compressed, func() error {
			gzipErr := compressed.Close()
			fileErr := file.Close()
			if gzipErr != nil {
				return gzipErr
			}
			return fileErr
		}, nil
	}
	return file, file.Close, nil
}

func unescape(value string) string {
	if value == "\\N" {
		return ""
	}
	return value
}

func splitList(value string) []string {
	value = unescape(value)
	if value == "" {
		return nil
	}
	result := strings.Split(value, ",")
	for index := range result {
		result[index] = strings.TrimSpace(result[index])
	}
	return result
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func parseYear(value string) *int {
	value = unescape(value)
	if value == "" {
		return nil
	}
	year, err := strconv.Atoi(value)
	if err != nil || year <= 0 {
		return nil
	}
	return &year
}

func parseInt(value string) int {
	parsed, err := strconv.Atoi(unescape(value))
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func parseInt64(value string) int64 {
	parsed, err := strconv.ParseInt(unescape(value), 10, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func parseFloat32(value string) float32 {
	parsed, err := strconv.ParseFloat(unescape(value), 32)
	if err != nil || parsed < 0 {
		return 0
	}
	return float32(parsed)
}
