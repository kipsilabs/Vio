package catalog

import "testing"

func TestValidateVirtualMediaCarriesSeasonPoster(t *testing.T) {
	valid := validVirtualSeries()
	valid.Episodes[0].SeasonPosterPath = "tvdb://banners/seasons/1.jpg"
	valid.Episodes[0].SeasonPosterSourcePath = "tvdb://banners/seasons/1.jpg"
	valid.Episodes[0].SeasonPosterThumbhash = "season-one-hash"
	if err := validateVirtualMedia(valid); err != nil {
		t.Fatalf("valid season poster rejected: %v", err)
	}

	invalid := validVirtualSeries()
	invalid.Episodes[0].SeasonPosterPath = " tvdb://banners/seasons/1.jpg "
	if err := validateVirtualMedia(invalid); err == nil {
		t.Fatal("season poster with surrounding whitespace was accepted")
	}
}
