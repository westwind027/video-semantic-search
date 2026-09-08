package metadata

import (
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCatalogFiltersAndJoinsIMDbDatasets(t *testing.T) {
	directory := t.TempDir()
	basicsContent := "tconst\ttitleType\tprimaryTitle\toriginalTitle\tisAdult\tstartYear\tendYear\truntimeMinutes\tgenres\n" +
		"tt0001\tmovie\tA Movie\tA Movie\t0\t2024\t\\N\t120\tDrama,Comedy\n" +
		"tt0002\tshort\tNot A Movie\tNot A Movie\t0\t2024\t\\N\t10\tDrama\n" +
		"tt0003\tmovie\tAdult Movie\tAdult Movie\t1\t2024\t\\N\t90\tDrama\n"
	basics := writeDataset(t, directory, "title.basics.tsv.gz", basicsContent)
	ratings := writeDataset(t, directory, "title.ratings.tsv.gz", "tconst\taverageRating\tnumVotes\n"+
		"tt0001\t8.7\t12345\n")
	principals := writeDataset(t, directory, "title.principals.tsv.gz", "tconst\tordering\tnconst\tcategory\tjob\tcharacters\n"+
		"tt0001\t1\tnm0001\tactor\t\\N\t[\\\"Hero\\\"]\n"+
		"tt0001\t2\tnm0002\tdirector\t\\N\t[\\\"Ignored\\\"]\n")
	names := writeDataset(t, directory, "name.basics.tsv.gz", "nconst\tprimaryName\tbirthYear\tdeathYear\tprimaryProfession\tknownForTitles\n"+
		"nm0001\tActor One\t1980\t\\N\tactor,producer\ttt0001\n")
	akas := writeDataset(t, directory, "title.akas.tsv.gz", "titleId\tordering\ttitle\tregion\tlanguage\ttypes\tattributes\tisOriginalTitle\n"+
		"tt0001\t1\tA Movie Alternate\tUS\ten\t\\N\t\\N\t0\n")
	crew := writeDataset(t, directory, "title.crew.tsv.gz", "tconst\tdirectors\twriters\n"+
		"tt0001\tnm0003\tnm0004,nm0005\n")

	year := 2024
	catalog, err := LoadCatalog(context.Background(), IMDbDatasetPaths{Basics: basics, Ratings: ratings, Principals: principals, Names: names, Akas: akas, Crew: crew}, IMDbImportOptions{
		TitleIDs: map[string]struct{}{"tt0001": {}},
		MinYear:  2020,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Titles) != 1 || catalog.Titles[0].ID != "tt0001" {
		t.Fatalf("titles = %+v", catalog.Titles)
	}
	if catalog.Titles[0].StartYear == nil || *catalog.Titles[0].StartYear != year || catalog.Titles[0].AverageRating != 8.7 || catalog.Titles[0].VoteCount != 12345 {
		t.Fatalf("title metadata = %+v", catalog.Titles[0])
	}
	if len(catalog.Principals["tt0001"]) != 1 || catalog.Principals["tt0001"][0].NameID != "nm0001" {
		t.Fatalf("principals = %+v", catalog.Principals)
	}
	if catalog.Names["nm0001"].PrimaryName != "Actor One" {
		t.Fatalf("names = %+v", catalog.Names)
	}
	if len(catalog.Titles[0].AlternateTitles) != 1 || catalog.Titles[0].AlternateTitles[0] != "A Movie Alternate" || len(catalog.Titles[0].Directors) != 1 || len(catalog.Titles[0].Writers) != 2 {
		t.Fatalf("extra title metadata = %+v", catalog.Titles[0])
	}
}

func writeDataset(t *testing.T, directory, name, content string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := gzip.NewWriter(file)
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
