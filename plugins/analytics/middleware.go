package analytics

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/appbaseio-confidential/arc/internal/iplookup"
	"github.com/appbaseio-confidential/arc/internal/types/acl"
	"github.com/appbaseio-confidential/arc/internal/types/index"
	"github.com/appbaseio-confidential/arc/internal/util"
	"github.com/google/uuid"
)

const (
	XSearchQuery         = "X-Search-Query"
	XSearchId            = "X-Search-Id"
	XSearchFilters       = "X-Search-Filters"
	XSearchClick         = "X-Search-Click"
	XSearchClickPosition = "X-Search-Click-Position"
	XSearchConversion    = "X-Search-Conversion"
	XSearchCustomEvent   = "X-Search-Custom-Event"
)

type searchResponse struct {
	Took float64 `json:"took"`
	Hits struct {
		Total int `json:"total"`
		Hits  []struct {
			Source map[string]interface{} `json:"source"`
			Type   string                 `json:"type"`
			Id     string                 `json:"id"`
		} `json:"hits"`
	} `json:"hits"`
}

type mSearchResponse struct {
	Responses []searchResponse `json:"responses"`
}

func (a *Analytics) recorder(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctxACL := ctx.Value(acl.CtxKey)
		if ctxACL == nil {
			log.Printf("%s: unable to fetch from request context", logTag)
			util.WriteBackMessage(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		reqACL, ok := ctxACL.(*acl.ACL)
		if !ok {
			log.Printf("%s: unable to cast context acl %v to *acl.ACL", logTag, reqACL)
			util.WriteBackMessage(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		searchQuery := r.Header.Get(XSearchQuery)
		searchId := r.Header.Get(XSearchId)
		if *reqACL != acl.Search || (searchQuery == "" && searchId == "") {
			h(w, r)
			return
		}

		docId := searchId
		if searchId == "" {
			docId = uuid.New().String()
		}

		// serve using response recorder
		respRecorder := httptest.NewRecorder()
		h(respRecorder, r)

		// copy the response to writer
		for k, v := range respRecorder.Header() {
			w.Header()[k] = v
		}
		w.Header().Set(XSearchId, docId)
		w.WriteHeader(respRecorder.Code)
		w.Write(respRecorder.Body.Bytes())

		go a.recordResponse(docId, searchId, respRecorder, r)
	}
}

// TODO: For urls ending with _search or _msearch?
func (a *Analytics) recordResponse(docId, searchId string, respRecorder *httptest.ResponseRecorder, r *http.Request) {
	// read the response from elasticsearch
	respBody, err := ioutil.ReadAll(respRecorder.Body)
	if err != nil {
		log.Printf("%s: can't read response body: %v", logTag, err)
		return
	}
	respBody = bytes.Replace(respBody, []byte("_source"), []byte("source"), -1)
	respBody = bytes.Replace(respBody, []byte("_type"), []byte("type"), -1)
	respBody = bytes.Replace(respBody, []byte("_id"), []byte("id"), -1)

	var esResponse searchResponse
	if strings.Contains(r.RequestURI, "_msearch") {
		var m mSearchResponse
		err := json.Unmarshal(respBody, &m)
		if err != nil {
			log.Printf("%s: unable to unmarshal es reponse %s: %v", logTag, string(respBody), err)
			return
		}
		if len(m.Responses) > 0 {
			esResponse = m.Responses[0]
		}
	} else {
		err := json.Unmarshal(respBody, &esResponse)
		if err != nil {
			log.Printf("%s: unable to unmarshal es reponse %s: %v", logTag, string(respBody), err)
			return
		}
	}

	// record top 10 responses
	var hits []map[string]string
	for i := 0; i < 10; i++ {
		source := esResponse.Hits.Hits[i].Source
		raw, err := json.Marshal(source)
		if err != nil {
			log.Printf("%s: unable to marshal es response source %s: %v", logTag, source, err)
			continue
		}

		h := make(map[string]string)
		h["id"] = esResponse.Hits.Hits[i].Id
		h["type"] = esResponse.Hits.Hits[i].Type
		h["source"] = string(raw)
		hits = append(hits, h)
	}

	record := make(map[string]interface{})
	record["took"] = esResponse.Took
	if searchId == "" {
		record["indices"] = r.Context().Value(index.CtxKey).([]string) // TODO: error check?
		record["search_query"] = r.Header.Get(XSearchQuery)
		record["hits_in_response"] = hits
		record["total_hits"] = esResponse.Hits.Total
		record["datestamp"] = time.Now().Format("2006/01/02 15:04:05")

		searchFilters := parse(r.Header.Get(XSearchFilters))
		if len(searchFilters) > 0 {
			record["search_filters"] = searchFilters
		}
	}

	ipAddr := r.Header.Get("X-Forwarded-For")
	record["ip"] = ipAddr

	ipInfo := iplookup.New()
	coordinates, err := ipInfo.GetCoordinates(ipAddr)
	if err != nil {
		log.Printf("%s: error fetching location coordinates for ip=%s: %v", logTag, ipAddr, err)
	} else {
		record["location"] = coordinates
	}
	country, err := ipInfo.Get(iplookup.Country, ipAddr)
	if err != nil {
		log.Printf("%s: error fetching country for ip=%s: %v", logTag, ipAddr, err)
	} else {
		record["country"] = country
	}

	searchClick := r.Header.Get(XSearchClick)
	if searchClick != "" {
		if clicked, err := strconv.ParseBool(searchClick); err == nil {
			record["click"] = clicked
		} else {
			log.Printf("%s: invalid bool value '%v' passed for header %s: %v",
				logTag, searchClick, XSearchClick, err)
		}
	}

	searchClickPosition := r.Header.Get(XSearchClickPosition)
	if searchClickPosition != "" {
		if pos, err := strconv.Atoi(searchClickPosition); err == nil {
			record["click_position"] = pos
		} else {
			log.Printf("%s: invalid int value '%v' passed for header %s: %v",
				logTag, searchClickPosition, XSearchClickPosition, err)
		}
	}

	searchConversion := r.Header.Get(XSearchConversion)
	if searchClickPosition != "" {
		if conversion, err := strconv.ParseBool(searchClickPosition); err == nil {
			record["conversion"] = conversion
		} else {
			log.Printf("%s: invalid bool value '%v' passed for header %s: %v",
				logTag, searchConversion, XSearchConversion, err)
		}
	}

	customEvents := parse(r.Header.Get(XSearchCustomEvent))
	if len(customEvents) > 0 {
		record["custom_events"] = customEvents
	}

	a.es.indexRecord(docId, record)
}

func parse(header string) []map[string]string {
	var m []map[string]string
	tokens := strings.Split(header, ",")
	for _, token := range tokens {
		values := strings.Split(token, "=")
		if len(values) == 2 {
			m = append(m, map[string]string{
				"key":   values[0],
				"value": values[1],
			})
		} else {
			log.Printf("%s: invalid value '%v' passed for header %s", logTag, token, XSearchFilters)
		}
	}
	return m
}