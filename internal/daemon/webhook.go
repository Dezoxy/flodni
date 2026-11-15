package daemon

import (
	"encoding/json"
	"errors"
	torrstor "flodni/internal/gostorm/torr/storage/torrstor"
	"flodni/internal/gostorm/web"
	"flodni/internal/warmup"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// reImdbID matches "imdb://tt1234567" in the Guid array of Plex webhook payloads.
var reImdbID = regexp.MustCompile(`"imdb://(tt\d+)"`)

// webhookMaxBody bounds a webhook post. Plex sends multipart with a thumbnail, so
// the ceiling has to clear a poster, not just the JSON.
const webhookMaxBody = 10 * 1024 * 1024

// handlePlexWebhook gestisce i messaggi in arrivo dal server Plex
func handlePlexWebhook(w http.ResponseWriter, r *http.Request) {
	logger.Info(fmt.Sprintf("[PLEX] Webhook connection from %s", r.RemoteAddr), "remote_addr", r.RemoteAddr)

	var payloadStr string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
		if err != nil {
			http.Error(w, "Bad request", 400)
			return
		}
		payloadStr = string(body)
	} else {
		// A form post is a form post: the plugin may send multipart or the older
		// application/x-www-form-urlencoded, and ParseMultipartForm reports the
		// latter as ErrNotMultipart after ParseForm has already filled the values.
		//
		// The cap matters for the urlencoded branch: ParseForm reads that body with
		// ReadAll, unbounded, and this process shares a 2200MB limit with the engine.
		//
		// ParseForm runs first on purpose: ParseMultipartForm swallows its error for a
		// urlencoded body and reports ErrNotMultipart instead, so an oversized post
		// would otherwise be read until the cap and then answer 200.
		//
		// The cap matches the multipart limit below rather than undercutting it: this
		// reader wraps the body before either branch, so a smaller value here would
		// reject a Plex webhook whose multipart carries a thumbnail, and a rejected
		// webhook is a playback session that never gets confirmed.
		r.Body = http.MaxBytesReader(w, r.Body, webhookMaxBody)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", 400)
			return
		}
		if err := r.ParseMultipartForm(webhookMaxBody); err != nil && !errors.Is(err, http.ErrNotMultipart) {
			http.Error(w, "Bad request", 400)
			return
		}
		payloadStr = r.FormValue("payload")
	}
	if payloadStr == "" {
		// Silent 200s make a misconfigured plugin a mystery: say so once per event.
		logger.Info(fmt.Sprintf("[PLEX] Webhook from %s carried no payload", r.RemoteAddr), "remote_addr", r.RemoteAddr)
		return
	}

	// Logga il payload per debug (limitato ai primi 500 caratteri per non intasare)
	displayPayload := payloadStr
	if len(displayPayload) > 500 {
		displayPayload = displayPayload[:500] + "..."
	}
	logger.Debug(fmt.Sprintf("[DEBUG] Webhook received: %s", displayPayload), "display_payload", displayPayload)

	var payload struct {
		Event    string `json:"event"`
		Metadata struct {
			Title              string `json:"title"`
			GrandparentTitle   string `json:"grandparentTitle"` // for TV series
			Year               optInt `json:"year"`             // a template sender may quote it
			ParentIndex        optInt `json:"parentIndex"`      // season (Plex)
			Index              optInt `json:"index"`            // episode number (Plex)
			LibrarySectionType string `json:"librarySectionType"`
			Media              []struct {
				Part []struct {
					File string `json:"file"`
				} `json:"Part"`
			} `json:"Media"`
		} `json:"Metadata"`
	}

	if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
		// Silent drops make a misconfigured sender look like no sender.
		logger.Info(fmt.Sprintf("[PLEX] Webhook payload does not parse from %s: %v", r.RemoteAddr, err), "remote_addr", r.RemoteAddr, "err", err)
		return
	}

	switch payload.Metadata.LibrarySectionType {
	case "movie", "show":
	default:
		return
	}

	if payload.Event == "media.play" || payload.Event == "media.resume" {
		targetTitle := strings.ToLower(payload.Metadata.Title)
		seriesTitle := strings.ToLower(payload.Metadata.GrandparentTitle)
		snap := app.linked.snapshot() // one read of the set for the match and the mark
		targetYear := payload.Metadata.Year.v

		logger.Debug(fmt.Sprintf("[DEBUG] Webhook for '%s' / '%s' (%d). Current registry:", targetTitle, seriesTitle, targetYear), "target_title", targetTitle, "series_title", seriesTitle, "target_year", targetYear)
		app.playbackRegistry.Range(func(key, value interface{}) bool {
			logger.Info(fmt.Sprintf("  - Registered: %s", key.(string)))
			return true
		})

		// Two-pass matching: exact first (filename, IMDB), fuzzy only as fallback.
		// The hash suffix is not a key: season-pack episodes share it (HashPrefix =
		// hash[:8]), so a suffix-only match can bind the wrong episode.

		// Extract IMDB ID via regex on raw payload (struct unmarshal would cause UnmarshalTypeError
		// due to Plex sending both lowercase "guid" and capital "Guid" fields).
		webhookImdbID := ""
		if m := reImdbID.FindStringSubmatch(payloadStr); len(m) > 1 {
			webhookImdbID = m[1]
		}

		// Pass 1: Exact matches only (filename, IMDB ID).
		var basenames, files []string
		for _, m := range payload.Metadata.Media {
			for _, p := range m.Part {
				files = append(files, p.File)
				if b := filepath.Base(p.File); b != "" && b != "." {
					basenames = append(basenames, b)
				}
			}
		}
		// plex_path_map: library symlinks into the mount, resolved only when no file
		// name matched; nil with the key empty.
		mappedNames := lazyPathMapStubNames(files)
		exactMatch, exactState, matchRule := findExactMatchRule(
			exactMatchKeys{imdbID: webhookImdbID, basenames: basenames, mappedBasenames: mappedNames, linked: snap},
			snapshotPlaybackEntries(),
		)

		// Pass 1c: IMDB bootstrap — if webhookImdbID is available but no state has it yet,
		// find the unique registered path of the matching library type with empty ImdbID.
		// One-time bootstrap: saves webhookImdbID into state so future sessions match via 0a.
		if exactMatch == "" && webhookImdbID != "" {
			sectionDir := ""
			switch payload.Metadata.LibrarySectionType {
			case "show":
				sectionDir = "/tv/"
			case "movie":
				sectionDir = "/movies/"
			}
			if sectionDir != "" {
				var bootPath string
				var bootState *PlaybackState
				bootCount := 0
				app.playbackRegistry.Range(func(key, value interface{}) bool {
					path := key.(string)
					state := value.(*PlaybackState)
					if strings.Contains(path, sectionDir) && state.GetImdbID() == "" {
						bootPath = path
						bootState = state
						bootCount++
					}
					return true
				})
				if bootCount == 1 {
					exactMatch = bootPath
					exactState = bootState
					matchRule = ruleIMDB
				}
			}
		}

		// Pass 2: Fuzzy matches only if no exact match found.
		if exactMatch == "" {
			var bestMatch string
			var bestState *PlaybackState
			bestLevel := 0             // higher = better match
			var ties []exactMatchEntry // every entry at bestLevel

			app.playbackRegistry.Range(func(key, value interface{}) bool {
				path := key.(string)
				filename := strings.ToLower(filepath.Base(path))
				level := 0

				// Tentativo 2: Match per Titolo completo (dot and underscore separators)
				if targetTitle != "" && (strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", ".")) ||
					strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", "_"))) {
					level = 3
				}

				// Tentativo 3: Match per Titolo Serie (TV Shows)
				if level == 0 && seriesTitle != "" {
					if strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", ".")) ||
						strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", "_")) {
						level = 2
					} else {
						words := strings.Fields(seriesTitle)
						if len(words) > 0 && len(words[0]) > 4 {
							cleanWord := strings.TrimRight(words[0], ":.,;!?")
							if len(cleanWord) > 4 && strings.Contains(filename, cleanWord) {
								level = 1
							}
						}
					}
				}

				// Tentativo 4: Match per prima parola + anno (first word > 3 chars to avoid "the", "a")
				if level == 0 && len(strings.Fields(targetTitle)) > 0 {
					firstWord := strings.Fields(targetTitle)[0]
					if len(firstWord) > 3 && strings.Contains(filename, firstWord) {
						if targetYear > 0 && strings.Contains(filename, strconv.Itoa(targetYear)) {
							level = 1
						}
					}
				}

				if level > bestLevel {
					bestLevel = level
					bestMatch = path
					bestState = value.(*PlaybackState)
					ties = ties[:0]
				}
				if level > 0 && level == bestLevel {
					ties = append(ties, exactMatchEntry{path: path, state: value.(*PlaybackState)})
				}
				return true
			})
			// A title tie with a known year keeps the files of that year ("Alien" is a
			// substring of "Aliens"); it only narrows, never adds a candidate.
			if ties = withYear(ties, payload.Metadata.Year); len(ties) > 0 {
				bestMatch, bestState = ties[0].path, ties[0].state
			}
			exactMatch = bestMatch
			exactState = bestState
			if exactMatch != "" {
				matchRule = ruleFuzzy
				// Several files fit equally well: the one a library symlink points at.
				if e, ok := snap.pickEntry(sameItem(ties, payload.Metadata.LibrarySectionType, payload.Metadata.ParentIndex, payload.Metadata.Index, payload.Metadata.Year)); ok {
					exactMatch, exactState, matchRule = e.path, e.state, ruleLinked
				}
			}
		}

		// Counted after the library-type filter: a movie or episode play or resume,
		// matched when it resolved to the one file that gets confirmed below.
		countPlaybackWebhook(exactMatch != "" && exactState != nil)
		if exactMatch == "" || exactState == nil {
			matchRule = ruleNone
		}
		app.playbackWebhookRules[matchRule].Add(1)
		if exactMatch != "" && exactState != nil {
			wasPlaying := exactState.GetStatus()
			exactState.SetHealthy(true)
			noteConfirmedLate(exactMatch)
			if !wasPlaying {
				// A confirmed start. What gets marked played is decided separately and
				// narrowly (played_mark.go); it never changes which file plays.
				chosen, ok := choosePlayedFile(playedKeys{
					basenames: basenames, mappedBasenames: mappedNames, imdbID: webhookImdbID,
					section: payload.Metadata.LibrarySectionType,
					title:   targetTitle, seriesTitle: seriesTitle, year: targetYear,
					season: payload.Metadata.ParentIndex, episode: payload.Metadata.Index,
					linked: snap,
				}, playedCandidates())
				countConfirmedPlay(chosen.path, chosen.key, ok)
				if ok && warmNext != nil {
					go warmNext.onConfirmedPlay(chosen.key) // off the webhook's path: it lists the library
				}
			}
			exactState.mu.Lock()
			exactState.IsStopped = false
			// Cache webhookImdbID into state for fast IMDB matching in future sessions.
			// A pass-1 match is precise (filename or IMDB ID) and cannot cross-pollinate
			// episodes of a season pack; the pass-1c/fuzzy fallbacks are less precise.
			if exactState.ImdbID == "" && webhookImdbID != "" {
				exactState.ImdbID = webhookImdbID
				logger.Info(fmt.Sprintf("[PLEX] IMDB ID cached for future matching: %s → %s", filepath.Base(exactMatch), webhookImdbID), "file", filepath.Base(exactMatch), "webhook_imdb_i_d", webhookImdbID)
			}
			exactState.mu.Unlock()
			invalidateConfirmed() // after IsStopped is cleared: the keep-alive sees the confirmation at once
			logger.Info(fmt.Sprintf("[PLEX] Playback confirmed by webhook for: %s", filepath.Base(exactMatch)), "file", filepath.Base(exactMatch))

			if exactState.Hash != "" {
				h := metainfo.NewHashFromHex(exactState.Hash)
				if t := web.BTS.GetTorrent(h); t != nil {
					t.IsPriority.Store(true)
					t.SetAggressiveMode(true, GetEffectiveConcurrencyLimit())
					logger.Info(fmt.Sprintf("[PLEX] High Priority + Aggressive Mode for: %s", exactState.Hash), "hash", exactState.Hash)
				}
			}
		}
		// After the confirmation, never before it: this stamp writes to SQLite when its
		// hourly write is due (usually the first webhook of a play), and a read held by
		// budgetGate waits for SetHealthy above, not for the database.
		webhookStatus.noteWebhook(exactMatch != "" && exactState != nil)
	} else if payload.Event == "media.stop" {
		targetTitle := strings.ToLower(payload.Metadata.Title)
		seriesTitle := strings.ToLower(payload.Metadata.GrandparentTitle)
		snap := app.linked.snapshot() // one read of the set for the match and the mark
		targetYear := payload.Metadata.Year.v

		stopImdbID := ""
		if m := reImdbID.FindStringSubmatch(payloadStr); len(m) > 1 {
			stopImdbID = m[1]
		}

		var basenames, files []string
		for _, m := range payload.Metadata.Media {
			for _, p := range m.Part {
				files = append(files, p.File)
				if b := filepath.Base(p.File); b != "" && b != "." {
					basenames = append(basenames, b)
				}
			}
		}

		// Pass 1: Exact matches (filename, plex_path_map stub name, IMDB ID) — never the
		// shared hash suffix.
		stopMatch, stopState, stopRule := findExactMatchRule(
			exactMatchKeys{imdbID: stopImdbID, basenames: basenames, mappedBasenames: lazyPathMapStubNames(files), linked: snap},
			snapshotPlaybackEntries(),
		)

		stopMatchFromFuzzy := false

		// Pass 2: Fuzzy matches only if no exact match.
		if stopMatch == "" {
			bestLevel := 0
			var ties []exactMatchEntry // every entry at bestLevel
			app.playbackRegistry.Range(func(key, value interface{}) bool {
				path := key.(string)
				filename := strings.ToLower(filepath.Base(path))
				level := 0

				if targetTitle != "" && (strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", ".")) ||
					strings.Contains(filename, strings.ReplaceAll(targetTitle, " ", "_"))) {
					level = 3
				}

				if level == 0 && seriesTitle != "" {
					if strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", ".")) ||
						strings.Contains(filename, strings.ReplaceAll(seriesTitle, " ", "_")) {
						level = 2
					} else {
						words := strings.Fields(seriesTitle)
						if len(words) > 0 && len(words[0]) > 4 {
							cleanWord := strings.TrimRight(words[0], ":.,;!?")
							if len(cleanWord) > 4 && strings.Contains(filename, cleanWord) {
								level = 1
							}
						}
					}
				}

				if level == 0 && len(strings.Fields(targetTitle)) > 0 {
					firstWord := strings.Fields(targetTitle)[0]
					if len(firstWord) > 3 && strings.Contains(filename, firstWord) {
						if targetYear > 0 && strings.Contains(filename, strconv.Itoa(targetYear)) {
							level = 1
						}
					}
				}

				if level > bestLevel {
					bestLevel = level
					stopMatch = path
					stopState = value.(*PlaybackState)
					stopMatchFromFuzzy = true
					ties = ties[:0]
				}
				if level > 0 && level == bestLevel {
					ties = append(ties, exactMatchEntry{path: path, state: value.(*PlaybackState)})
				}
				return true
			})
			if ties = withYear(ties, payload.Metadata.Year); len(ties) > 0 {
				stopMatch, stopState = ties[0].path, ties[0].state
			}
			// Several files fit equally well: the one a library symlink points at.
			if e, ok := snap.pickEntry(sameItem(ties, payload.Metadata.LibrarySectionType, payload.Metadata.ParentIndex, payload.Metadata.Index, payload.Metadata.Year)); ok {
				stopMatch, stopState, stopRule, stopMatchFromFuzzy = e.path, e.state, ruleLinked, false
			}
		}

		// Only a basename match (pass 1) is the file by construction. An IMDB-only match
		// is precise on this deployment (episode-level IDs) but only when the registry
		// holds exactly one entry with that ID; otherwise it is a guess like fuzzy.
		matchByBasename := stopRule == rulePathMap // the stub name of a library symlink
		if stopMatch != "" {
			base := filepath.Base(stopMatch)
			for _, b := range basenames {
				if b == base {
					matchByBasename = true
					break
				}
			}
		}
		// A linked match is trusted only when the file is the episode or the movie year the webhook says.
		linkedStop := stopRule == ruleLinked && fileAgrees(stopMatch, payload.Metadata.LibrarySectionType, payload.Metadata.ParentIndex, payload.Metadata.Index, payload.Metadata.Year)
		imdbUnique := false
		if stopMatch != "" && !matchByBasename && !stopMatchFromFuzzy && stopImdbID != "" {
			n := 0
			app.playbackRegistry.Range(func(_, v interface{}) bool {
				ps, ok := v.(*PlaybackState)
				if !ok || ps == nil {
					return true
				}
				if ps.GetImdbID() == stopImdbID {
					n++
				}
				return n <= 1
			})
			imdbUnique = n == 1
		}
		// A fuzzy (pass 2) match is a name-similarity guess; an IMDB-only match is only
		// trusted when unique. If such a match still has a live handle it is likely a
		// sibling episode being read, so a late or spurious stop must not kill it.
		// Basename and unique-IMDB matches are honored even with the handle open:
		// ignoring them would leave the pump alive until the 2h idle timeout (V262).
		// So is a linked match: the one stub among several that a library symlink points at.
		if stopMatch != "" && stopState != nil && !matchByBasename && !imdbUnique && !linkedStop && anyLiveHandleFor(stopMatch) {
			logger.Info(fmt.Sprintf("[PLEX] STOP ignored for %s: fuzzy or ambiguous match with handle still open", filepath.Base(stopMatch)), "file", filepath.Base(stopMatch))
		} else if stopMatch != "" && stopState != nil {
			stopState.mu.Lock()
			stopState.IsStopped = true
			stopState.mu.Unlock()
			stopState.SetHealthy(false) // persists with IsStopped=true
			invalidateConfirmed()       // the keep-alive releases the torrent at once
			logger.Info(fmt.Sprintf("[PLEX] STOP applied for: %s (Event: %s)", filepath.Base(stopMatch), payload.Event), "file", filepath.Base(stopMatch), "event", payload.Event)

			if val, ok := app.activePumps.Load(stopMatch); ok {
				ps := val.(*NativePumpState)
				if ps.cancel != nil {
					ps.cancel()
				}
				app.activePumps.Delete(stopMatch)
				logger.Info(fmt.Sprintf("[PLEX] STOP: force-terminated pump for %s", filepath.Base(stopMatch)), "file", filepath.Base(stopMatch))
			}

			torrstor.ResetShield()
			logger.Info("[AdaptiveShield] Shield reset on media.stop")

			// Pull the rest of the tail window now that the pump is gone: piece requests at
			// the file end would otherwise compete with the sequential download front. Ready
			// for the next open, which is when the MKV Cues probe needs it.
			if warmup.DiskWarmup != nil {
				if v, ok := tailFillTargets.Load(stopMatch); ok {
					t := v.(tailFillTarget)
					warmup.DiskWarmup.EnsureTail(t.hash, t.fileID, t.size)
				}
			}

			// Deactivate Core Priority unless a sibling episode of the same torrent is
			// still live (shared hash): priority/aggressive off and the 30s expiry would
			// hit the stream that is actually playing.
			if shouldDropTorrentPriority(stopState.Hash, stopMatch) {
				h := metainfo.NewHashFromHex(stopState.Hash)
				if t := web.BTS.GetTorrent(h); t != nil {
					t.IsPriority.Store(false)
					t.SetAggressiveMode(false, 0) // Back to normal download priority
					t.AddExpiredTime(30 * time.Second)
					logger.Info(fmt.Sprintf("[PLEX] STOP detected. Grace period 30s for: %s", stopState.Hash), "hash", stopState.Hash)
				}
			}
		}
	}
	w.WriteHeader(200)
}
