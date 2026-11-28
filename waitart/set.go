package waitart

import (
	"fmt"
	"math/rand/v2"
)

// Kind is the kind of wait a scene is chosen for, such as a build or a wait on a person. A product
// chooses its own kinds; the package gives none a meaning.
type Kind string

// Set is a fixed list of scenes and the kinds of wait each fits. A Set is not changed after [NewSet]
// returns it, so it is safe to share between goroutines.
type Set struct {
	scenes []Scene
	kinds  map[Kind][]int
}

// NewSet checks every scene with [Scene.Validate] and returns a set that holds a copy of them. kinds
// maps a kind of wait to the names of the scenes that fit it; a kind it does not list, or lists with no
// names, gets any scene of the set. An invalid scene gives an error that matches [ErrInvalidScene]; an
// empty list, a name used twice or a kind that names a missing scene gives [ErrInvalidSet].
func NewSet(scenes []Scene, kinds map[Kind][]string) (*Set, error) {
	if len(scenes) == 0 {
		return nil, fmt.Errorf("%w: no scenes", ErrInvalidSet)
	}
	s := &Set{scenes: make([]Scene, 0, len(scenes)), kinds: make(map[Kind][]int, len(kinds))}
	index := make(map[string]int, len(scenes))
	for i, sc := range scenes {
		if err := sc.Validate(); err != nil {
			return nil, fmt.Errorf("scene %d: %w", i, err)
		}
		if _, dup := index[sc.Name]; dup {
			return nil, fmt.Errorf("%w: scene %d reuses a name", ErrInvalidSet, i)
		}
		index[sc.Name] = i
		s.scenes = append(s.scenes, sc.clone())
	}
	for k, names := range kinds {
		for _, n := range names {
			i, ok := index[n]
			if !ok {
				return nil, fmt.Errorf("%w: a kind names a scene the set does not hold", ErrInvalidSet)
			}
			s.kinds[k] = append(s.kinds[k], i)
		}
	}
	return s, nil
}

// Scenes returns copies of the scenes of the set in the order they were given; changing them does
// not change the set.
func (s *Set) Scenes() []Scene {
	out := make([]Scene, len(s.scenes))
	for i, sc := range s.scenes {
		out[i] = sc.clone()
	}
	return out
}

// Lookup returns a copy of the scene with the given name; changing it does not change the set.
func (s *Set) Lookup(name string) (Scene, bool) {
	for _, sc := range s.scenes {
		if sc.Name == name {
			return sc.clone(), true
		}
	}
	return Scene{}, false
}

// ForKind returns a copy of a scene that fits the kind of wait, or of any scene of the set for a kind
// with none of its own; changing it does not change the set. src picks among the fitting scenes; with
// the same source state the choice is the same, so a test passes a seeded source. A nil src picks the
// first fitting scene. ForKind is safe to call from several goroutines when src is.
func (s *Set) ForKind(k Kind, src rand.Source) Scene {
	fit := s.kinds[k]
	n := len(fit)
	if n == 0 {
		n = len(s.scenes)
	}
	i := 0
	if src != nil {
		i = int(src.Uint64() % uint64(n)) // #nosec G115 -- the remainder is below n, which is an int
	}
	if len(fit) > 0 {
		i = fit[i]
	}
	return s.scenes[i].clone()
}
