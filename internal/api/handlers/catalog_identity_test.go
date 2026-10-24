package handlers

import (
	"context"
	"testing"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/models"
)

type rematchedIdentityItems struct{ providerOnly bool }

func (rematchedIdentityItems) ResolveIdentities(context.Context, catalog.IdentityRequest, catalog.AccessFilter) (catalog.IdentityResult, error) {
	return catalog.IdentityResult{Matches: []catalog.IdentityMatch{{Seed: 0, ContentID: "native-rematched"}}}, nil
}
func (f rematchedIdentityItems) GetByIDsWithAccess(context.Context, []string, catalog.AccessFilter) ([]*models.MediaItem, error) {
	id := "999"
	if f.providerOnly {
		id = ""
	}
	return []*models.MediaItem{{ContentID: "native-rematched", Type: "movie", TmdbID: id, Title: "Synthetic card"}}, nil
}
func (f rematchedIdentityItems) GetIdentityProviderIDs(context.Context, []string) (map[string][]*models.MediaItemProviderID, error) {
	if f.providerOnly {
		return map[string][]*models.MediaItemProviderID{"native-rematched": {{ContentID: "native-rematched", ItemType: "movie", Provider: "tmdb", ProviderID: "101"}}}, nil
	}
	return nil, nil
}
func TestResolveIdentitiesRejectsCardRematchedDuringHydration(t *testing.T) {
	h := &CatalogHandler{itemsH: &ItemsHandler{}, identityItems: rematchedIdentityItems{}}
	view, err := h.ResolveIdentities(t.Context(), ItemViewer{}, catalog.IdentityRequest{Seeds: []catalog.IdentitySeed{{Type: "movie", TmdbID: "101"}}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Matches) != 0 {
		t.Fatalf("confirmed rematched card: %+v", view.Matches)
	}
	h.identityItems = rematchedIdentityItems{providerOnly: true}
	view, err = h.ResolveIdentities(t.Context(), ItemViewer{}, catalog.IdentityRequest{Seeds: []catalog.IdentitySeed{{Type: "movie", TmdbID: "101", ImdbID: "tt101"}}, Limit: 100})
	if err != nil || len(view.Matches) != 1 {
		t.Fatalf("active provider match with missing namespace = %+v, %v", view, err)
	}

}
