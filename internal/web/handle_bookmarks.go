package web

import (
	"context"
	"net/http"
	"time"

	"lantern/internal/store"
	"lantern/internal/util"
)

func (s *Server) listBookmarks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.GetAllBookmarks())
}

func (s *Server) createBookmark(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid form data")
		return
	}
	bmURL := r.FormValue("url")
	if bmURL == "" {
		errorResponse(w, http.StatusBadRequest, "url is required")
		return
	}
	name := r.FormValue("name")
	if name == "" {
		name = bmURL
	}
	bm := &store.Bookmark{
		ID:       util.NewID(),
		Name:     name,
		URL:      bmURL,
		Category: r.FormValue("category"),
	}
	s.store.AddBookmark(bm)
	s.save()
	go s.fetchBookmarkFavicon(bm.ID)
	toastTrigger(w, "Bookmark added", "success", "refreshBookmarksTable")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateBookmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid form data")
		return
	}
	updated := &store.Bookmark{
		ID:       id,
		Name:     r.FormValue("name"),
		URL:      r.FormValue("url"),
		Category: r.FormValue("category"),
	}
	if !s.store.UpdateBookmark(id, updated) {
		errorResponse(w, http.StatusNotFound, "bookmark not found")
		return
	}
	s.save()
	go s.fetchBookmarkFavicon(id)
	toastTrigger(w, "Bookmark updated", "success", "refreshBookmarksTable")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteBookmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.store.DeleteBookmark(id) {
		apiError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	s.store.DeleteIcon(id)
	s.save()
	w.WriteHeader(http.StatusOK)
}

// fetchBookmarkFavicon asynchronously fetches and persists the favicon for a
// bookmark, then sets Icon = "file" so subsequent renders use the fast disk path.
func (s *Server) fetchBookmarkFavicon(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !util.FetchAndWriteFavicon(ctx, s.store, id) {
		return
	}
	if bm := s.store.GetBookmarkByID(id); bm != nil && bm.Icon != store.IconFile {
		updated := *bm
		updated.Icon = store.IconFile
		s.store.UpdateBookmark(id, &updated)
		s.save()
	}
}
