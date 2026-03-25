package web

import (
	"context"
	"log"
	"net/http"
	"time"

	"lantern/internal/store"
)

func (s *Server) listBookmarks(w http.ResponseWriter, r *http.Request) {
	bms := s.store.GetAllBookmarks()
	if bms == nil {
		bms = []*store.Bookmark{}
	}
	writeJSON(w, http.StatusOK, bms)
}

func (s *Server) createBookmark(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		errorTrigger(w, "invalid form data")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	bmURL := r.FormValue("url")
	if bmURL == "" {
		errorTrigger(w, "url is required")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	if name == "" {
		name = bmURL
	}
	bm := &store.Bookmark{
		ID:       newID(),
		Name:     name,
		URL:      bmURL,
		Category: r.FormValue("category"),
	}
	s.store.AddBookmark(bm)
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	go s.fetchBookmarkFavicon(bm.ID, bmURL)
	toastTrigger(w, "Bookmark added", "success", "refreshBookmarksTable")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateBookmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		errorTrigger(w, "invalid form data")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	updated := &store.Bookmark{
		ID:       id,
		Name:     r.FormValue("name"),
		URL:      r.FormValue("url"),
		Category: r.FormValue("category"),
	}
	if !s.store.UpdateBookmark(id, updated) {
		errorTrigger(w, "bookmark not found")
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	go s.fetchBookmarkFavicon(id, updated.URL)
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
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	w.WriteHeader(http.StatusOK)
}

// fetchBookmarkFavicon asynchronously fetches and persists the favicon for a
// bookmark, then sets Icon = "file" so subsequent renders use the fast disk path.
func (s *Server) fetchBookmarkFavicon(id, bmURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !fetchAndWriteFavicon(ctx, s.store, id, bmURL) {
		return
	}
	if bm := s.store.GetBookmarkByID(id); bm != nil && bm.Icon != "file" {
		updated := *bm
		updated.Icon = "file"
		s.store.UpdateBookmark(id, &updated)
		if err := s.store.Save(); err != nil {
			log.Printf("web: bookmark favicon save: %v", err)
		}
	}
}
