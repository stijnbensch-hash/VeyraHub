package main

import "time"

// PlaybackProgress is one user's resume position for one title (or, for a
// series, one episode — MediaID is the episode's own id, same as streams/
// PlaybackInfo use elsewhere). The hub never measures playback itself: the
// Veyra client reports its position periodically while playing and reads
// it back when starting playback, the same way ProfileUsage-derived screen
// time already works (see profiles.go).
type PlaybackProgress struct {
	UserID          string    `json:"userID"`
	MediaType       string    `json:"mediaType"`
	MediaID         string    `json:"mediaID"`
	PositionSeconds float64   `json:"positionSeconds"`
	DurationSeconds float64   `json:"durationSeconds,omitempty"`
	Finished        bool      `json:"finished,omitempty"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

const (
	// progressNearEndFraction: playback within this fraction of the
	// reported duration counts as finished (end credits rolling, or the
	// last buffered second never quite being reached) — a standard
	// resume-position convention, so the client doesn't have to say
	// "count this as done" itself.
	progressNearEndFraction = 0.95

	// progressMinResumeSeconds: below this, there's nothing meaningful to
	// resume — starting over is indistinguishable from resuming.
	progressMinResumeSeconds = 15
)

func (s *Store) progressIndexLocked(userID, mediaType, mediaID string) int {
	for i := range s.state.Progress {
		p := &s.state.Progress[i]
		if p.UserID == userID && p.MediaType == mediaType && p.MediaID == mediaID {
			return i
		}
	}
	return -1
}

// ProgressForUser returns the stored resume position for one title, if
// any. A finished title, or one with no meaningfully-resumable position,
// is reported as not found so the client just starts from the beginning.
func (s *Store) ProgressForUser(userID, mediaType, mediaID string) (PlaybackProgress, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	index := s.progressIndexLocked(userID, mediaType, mediaID)
	if index < 0 {
		return PlaybackProgress{}, false
	}
	progress := s.state.Progress[index]
	if progress.Finished || progress.PositionSeconds < progressMinResumeSeconds {
		return PlaybackProgress{}, false
	}
	return progress, true
}

// SetProgressForUser records a user's current resume position for one
// title. A position within progressNearEndFraction of durationSeconds
// (when duration is known) is treated as finished, so a completed title
// doesn't linger as "resume from 99%".
func (s *Store) SetProgressForUser(userID, mediaType, mediaID string, positionSeconds, durationSeconds float64) (PlaybackProgress, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	finished := durationSeconds > 0 && positionSeconds >= durationSeconds*progressNearEndFraction

	record := PlaybackProgress{
		UserID: userID, MediaType: mediaType, MediaID: mediaID,
		PositionSeconds: positionSeconds, DurationSeconds: durationSeconds,
		Finished: finished, UpdatedAt: time.Now().UTC(),
	}

	if index := s.progressIndexLocked(userID, mediaType, mediaID); index >= 0 {
		s.state.Progress[index] = record
	} else {
		s.state.Progress = append(s.state.Progress, record)
	}

	if err := s.persistLocked(); err != nil {
		return PlaybackProgress{}, err
	}
	return record, nil
}

// ClearProgressForUser removes any stored resume position for one title
// (e.g. the client explicitly restarted playback from the beginning).
func (s *Store) ClearProgressForUser(userID, mediaType, mediaID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	index := s.progressIndexLocked(userID, mediaType, mediaID)
	if index < 0 {
		return nil
	}
	s.state.Progress = append(s.state.Progress[:index], s.state.Progress[index+1:]...)
	return s.persistLocked()
}
