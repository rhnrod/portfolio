package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Estrutura para o cache de faixas
type TrackCacheItem struct {
	Album    string
	CoverURL string
	Expires  time.Time
}

type LastFMCache struct {
	sync.RWMutex
	items  map[string]TrackCacheItem
	apiKey string
}

func NewLastFMCache(apiKey string) *LastFMCache {
	return &LastFMCache{
		items:  make(map[string]TrackCacheItem),
		apiKey: apiKey,
	}
}

// Chave única para o cache baseada em Artista + Música
func (c *LastFMCache) getCacheKey(artist, track string) string {
	return artist + " - " + track
}

// Busca do cache ou consulta a API track.getInfo se não encontrar/estiver expirado
func (c *LastFMCache) GetOrFetch(artist, track string) (string, string) {
	key := c.getCacheKey(artist, track)

	c.RLock()
	item, found := c.items[key]
	c.RUnlock()

	// Se encontrou e não expirou (TTL de 24 horas), retorna direto do cache
	if found && time.Now().Before(item.Expires) {
		return item.Album, item.CoverURL
	}

	// Se não encontrou, busca no track.getInfo
	album, cover := c.fetchTrackInfoAPI(artist, track)

	c.Lock()
	c.items[key] = TrackCacheItem{
		Album:    album,
		CoverURL: cover,
		Expires:  time.Now().Add(24 * time.Hour), // Cache válido por 24h
	}
	c.Unlock()

	return album, cover
}

// Estruturas auxiliares para decodificar o JSON do track.getInfo
type TrackInfoResponse struct {
	Track struct {
		Album struct {
			Title string `json:"title"`
			Image []struct {
				Text string `json:"#text"`
				Size string `json:"size"`
			} `json:"image"`
		} `json:"album"`
	} `json:"track"`
}

func (c *LastFMCache) fetchTrackInfoAPI(artist, track string) (string, string) {
	apiURL := fmt.Sprintf(
		"https://ws.audioscrobbler.com/2.0/?method=track.getInfo&api_key=%s&artist=%s&track=%s&format=json",
		c.apiKey,
		url.QueryEscape(artist),
		url.QueryEscape(track),
	)

	resp, err := http.Get(apiURL)
	if err != nil || resp.StatusCode != 200 {
		return "", ""
	}
	defer resp.Body.Close()

	var result TrackInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", ""
	}

	albumTitle := result.Track.Album.Title

	// Pega a imagem de maior tamanho disponível (geralmente 'extralarge' ou a última do array)
	coverURL := ""
	for _, img := range result.Track.Album.Image {
		if img.Text != "" {
			coverURL = img.Text
		}
	}

	return albumTitle, coverURL
}
