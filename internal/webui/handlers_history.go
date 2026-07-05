package webui

import (
	"fmt"
	"net/http"
)

type historyRowView struct {
	MovedAt  string
	Title    string
	ArrApp   string
	FromTier string
	FromPath string
	ToTier   string
	ToPath   string
	Size     string
}

type historyData struct {
	Title string
	Error string
	Rows  []historyRowView
}

func (s *Server) handleHistoryPage(w http.ResponseWriter, r *http.Request) {
	data := historyData{Title: "History"}

	eng, err := s.newEngine()
	if err != nil {
		data.Error = err.Error()
		s.render(w, "history", data)
		return
	}

	for _, rec := range eng.History.All() {
		data.Rows = append(data.Rows, historyRowView{
			MovedAt:  rec.MovedAt.Format("2006-01-02 15:04"),
			Title:    rec.Title,
			ArrApp:   rec.ArrApp,
			FromTier: rec.FromTier,
			FromPath: rec.FromPath,
			ToTier:   rec.ToTier,
			ToPath:   rec.ToPath,
			Size:     fmt.Sprintf("%.1f GB", float64(rec.SizeBytes)/(1<<30)),
		})
	}

	s.render(w, "history", data)
}
