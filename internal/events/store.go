package events

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/17xande-dev/goevent/internal/db/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned for an event, ticket type or question that does not
// exist, so handlers can answer 404 without inspecting driver errors.
var ErrNotFound = errors.New("events: not found")

// ErrInUse is returned when a delete would orphan rows history points at —
// registrations under an event, attendees holding a ticket type, answers to a
// question.
var ErrInUse = errors.New("events: still referenced by registrations")

// ErrSlugTaken means another event already has that address.
var ErrSlugTaken = errors.New("events: that slug is already taken")

// ErrStatusMove is a status change not available from where the event is — or
// no longer available, because somebody else moved it first.
var ErrStatusMove = errors.New("events: that status change is not available")

// ErrNothingOffered refuses publishing an event with no ticket type anybody
// could register for.
var ErrNothingOffered = errors.New("events: no ticket type is on offer")

// Store is the persistence for events, their ticket types and their questions.
// The SQL is in internal/db/queries/events.sql; what is here is the translation
// between the driver's vocabulary and the domain's.
type Store struct {
	pool *pgxpool.Pool
	q    *gen.Queries
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: gen.New(pool)}
}

// List returns every event, newest first. The admin is the caller, and an
// organisation's events number in the dozens a year, so it is unpaginated.
func (s *Store) List(ctx context.Context) ([]Event, error) {
	rows, err := s.q.ListEvents(ctx)
	if err != nil {
		return nil, fmt.Errorf("events: list: %w", err)
	}
	out := make([]Event, len(rows))
	for i, r := range rows {
		out[i] = event(r)
	}
	return out, nil
}

func (s *Store) Get(ctx context.Context, id string) (Event, error) {
	r, err := s.q.GetEvent(ctx, id)
	if err != nil {
		return Event{}, translate(fmt.Errorf("events: get: %w", err))
	}
	return event(r), nil
}

func (s *Store) GetBySlug(ctx context.Context, slug string) (Event, error) {
	r, err := s.q.GetEventBySlug(ctx, slug)
	if err != nil {
		return Event{}, translate(fmt.Errorf("events: get by slug: %w", err))
	}
	return event(r), nil
}

// Create stores a new event as a draft. Status is not taken from e: an event is
// published by its own action, never by the details form.
func (s *Store) Create(ctx context.Context, e Event) (Event, error) {
	r, err := s.q.CreateEvent(ctx, gen.CreateEventParams{
		Slug: e.Slug, Title: e.Title, Summary: e.Summary, Description: e.Description,
		Venue: e.Venue, Address: e.Address, StartsAt: e.StartsAt, EndsAt: e.EndsAt,
		Timezone: e.Timezone, Capacity: e.Capacity, Listed: e.Listed,
		RegistrationOpensAt: e.RegistrationOpensAt, RegistrationClosesAt: e.RegistrationClosesAt,
	})
	if err != nil {
		return Event{}, translate(fmt.Errorf("events: create: %w", err))
	}
	return event(r), nil
}

// Update writes the details form. Status and image are deliberately not
// written here — see the query.
func (s *Store) Update(ctx context.Context, e Event) (Event, error) {
	r, err := s.q.UpdateEvent(ctx, gen.UpdateEventParams{
		ID: e.ID, Slug: e.Slug, Title: e.Title, Summary: e.Summary, Description: e.Description,
		Venue: e.Venue, Address: e.Address, StartsAt: e.StartsAt, EndsAt: e.EndsAt,
		Timezone: e.Timezone, Capacity: e.Capacity, Listed: e.Listed,
		RegistrationOpensAt: e.RegistrationOpensAt, RegistrationClosesAt: e.RegistrationClosesAt,
	})
	if err != nil {
		return Event{}, translate(fmt.Errorf("events: update: %w", err))
	}
	return event(r), nil
}

// Transition moves an event from one status to another, refusing in the same
// statement if it is no longer in from, if the move is not one from allows, or —
// for publishing — if nothing is on offer. See TransitionEvent.
func (s *Store) Transition(ctx context.Context, id string, from, to Status) (Event, error) {
	if !from.CanBecome(to) {
		return Event{}, ErrStatusMove
	}
	r, err := s.q.TransitionEvent(ctx, gen.TransitionEventParams{
		ID: id, FromStatus: string(from), NextStatus: string(to),
	})
	if err == nil {
		return event(r), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Event{}, translate(fmt.Errorf("events: transition: %w", err))
	}
	// Refused. Say which reason applies, from the row as it is now.
	cur, err := s.Get(ctx, id)
	switch {
	case err != nil:
		return Event{}, err
	case cur.Status != from:
		return Event{}, ErrStatusMove
	default:
		return Event{}, ErrNothingOffered
	}
}

// SetImage points the event at a stored image; an empty key clears it.
func (s *Store) SetImage(ctx context.Context, id, key string) (Event, error) {
	var k *string
	if key != "" {
		k = &key
	}
	r, err := s.q.SetEventImage(ctx, gen.SetEventImageParams{ID: id, ImageKey: k})
	if err != nil {
		return Event{}, translate(fmt.Errorf("events: set image: %w", err))
	}
	return event(r), nil
}

// Delete removes an event nobody has registered for. One with registrations is
// ErrInUse: it is cancelled, never deleted, because the registrations are a
// record of people and money.
func (s *Store) Delete(ctx context.Context, id string) error {
	n, err := s.q.DeleteEvent(ctx, id)
	if err != nil {
		return translate(fmt.Errorf("events: delete: %w", err))
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TicketTypes returns an event's ticket types, archived ones last.
func (s *Store) TicketTypes(ctx context.Context, eventID string) ([]TicketType, error) {
	rows, err := s.q.ListTicketTypes(ctx, eventID)
	if err != nil {
		return nil, translate(fmt.Errorf("events: list ticket types: %w", err))
	}
	out := make([]TicketType, len(rows))
	for i, r := range rows {
		out[i] = ticketType(r)
	}
	return out, nil
}

func (s *Store) TicketType(ctx context.Context, eventID, id string) (TicketType, error) {
	r, err := s.q.GetTicketType(ctx, gen.GetTicketTypeParams{EventID: eventID, ID: id})
	if err != nil {
		return TicketType{}, translate(fmt.Errorf("events: get ticket type: %w", err))
	}
	return ticketType(r), nil
}

func (s *Store) CreateTicketType(ctx context.Context, t TicketType) (TicketType, error) {
	r, err := s.q.CreateTicketType(ctx, gen.CreateTicketTypeParams{
		EventID: t.EventID, Name: t.Name, Description: t.Description, PriceCents: t.PriceCents,
		Capacity: t.Capacity, MaxPerRegistration: t.MaxPerRegistration,
		SalesStartAt: t.SalesStartAt, SalesEndAt: t.SalesEndAt, Hidden: t.Hidden, Position: t.Position,
	})
	if err != nil {
		return TicketType{}, translate(fmt.Errorf("events: create ticket type: %w", err))
	}
	return ticketType(r), nil
}

func (s *Store) UpdateTicketType(ctx context.Context, t TicketType) (TicketType, error) {
	r, err := s.q.UpdateTicketType(ctx, gen.UpdateTicketTypeParams{
		EventID: t.EventID, ID: t.ID, Name: t.Name, Description: t.Description, PriceCents: t.PriceCents,
		Capacity: t.Capacity, MaxPerRegistration: t.MaxPerRegistration,
		SalesStartAt: t.SalesStartAt, SalesEndAt: t.SalesEndAt, Hidden: t.Hidden,
		Position: t.Position, Archived: t.Archived,
	})
	if err != nil {
		return TicketType{}, translate(fmt.Errorf("events: update ticket type: %w", err))
	}
	return ticketType(r), nil
}

// RemoveTicketType deletes a ticket type nobody holds, and archives one that
// somebody does — attendees keep pointing at what they bought. archived says
// which happened, so the admin can say so.
func (s *Store) RemoveTicketType(ctx context.Context, eventID, id string) (archived bool, err error) {
	return s.remove(ctx,
		func(q *gen.Queries) (int64, error) {
			return q.DeleteTicketType(ctx, gen.DeleteTicketTypeParams{EventID: eventID, ID: id})
		},
		func(q *gen.Queries) (int64, error) {
			return q.ArchiveTicketType(ctx, gen.ArchiveTicketTypeParams{EventID: eventID, ID: id})
		})
}

// Questions returns an event's questions, archived ones last.
func (s *Store) Questions(ctx context.Context, eventID string) ([]Question, error) {
	rows, err := s.q.ListQuestions(ctx, eventID)
	if err != nil {
		return nil, translate(fmt.Errorf("events: list questions: %w", err))
	}
	out := make([]Question, len(rows))
	for i, r := range rows {
		out[i] = question(r)
	}
	return out, nil
}

func (s *Store) Question(ctx context.Context, eventID, id string) (Question, error) {
	r, err := s.q.GetQuestion(ctx, gen.GetQuestionParams{EventID: eventID, ID: id})
	if err != nil {
		return Question{}, translate(fmt.Errorf("events: get question: %w", err))
	}
	return question(r), nil
}

func (s *Store) CreateQuestion(ctx context.Context, q Question) (Question, error) {
	r, err := s.q.CreateQuestion(ctx, gen.CreateQuestionParams{
		EventID: q.EventID, Scope: string(q.Scope), Kind: string(q.Kind), Label: q.Label,
		Help: q.Help, Options: nonNil(q.Options), Required: q.Required,
		TicketTypeIDs: limit(q), Position: q.Position,
	})
	if err != nil {
		return Question{}, translate(fmt.Errorf("events: create question: %w", err))
	}
	return question(r), nil
}

func (s *Store) UpdateQuestion(ctx context.Context, q Question) (Question, error) {
	r, err := s.q.UpdateQuestion(ctx, gen.UpdateQuestionParams{
		EventID: q.EventID, ID: q.ID, Scope: string(q.Scope), Kind: string(q.Kind), Label: q.Label,
		Help: q.Help, Options: nonNil(q.Options), Required: q.Required,
		TicketTypeIDs: limit(q), Position: q.Position, Archived: q.Archived,
	})
	if err != nil {
		return Question{}, translate(fmt.Errorf("events: update question: %w", err))
	}
	return question(r), nil
}

// RemoveQuestion deletes a question nobody has answered and archives one that
// has answers, on the same terms as RemoveTicketType.
func (s *Store) RemoveQuestion(ctx context.Context, eventID, id string) (archived bool, err error) {
	return s.remove(ctx,
		func(q *gen.Queries) (int64, error) {
			return q.DeleteQuestion(ctx, gen.DeleteQuestionParams{EventID: eventID, ID: id})
		},
		func(q *gen.Queries) (int64, error) {
			return q.ArchiveQuestion(ctx, gen.ArchiveQuestionParams{EventID: eventID, ID: id})
		})
}

// remove tries a delete and falls back to archiving when history refers to the
// row. The delete runs under a savepoint, because a foreign-key violation aborts
// the transaction it happens in and the archive has to run after it.
//
// Trying is the honest way to ask. Counting references first and then deleting
// is two statements with a registration able to land between them.
func (s *Store) remove(ctx context.Context, del, archive func(*gen.Queries) (int64, error)) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("events: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := s.q.WithTx(tx)

	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("events: savepoint: %w", err)
	}
	n, err := del(gen.New(sp))
	switch {
	case err == nil:
		if err := sp.Commit(ctx); err != nil {
			return false, fmt.Errorf("events: release savepoint: %w", err)
		}
		if n == 0 {
			return false, ErrNotFound
		}
		return false, tx.Commit(ctx)
	case !errors.Is(translate(err), ErrInUse):
		return false, translate(fmt.Errorf("events: delete: %w", err))
	}
	if err := sp.Rollback(ctx); err != nil {
		return false, fmt.Errorf("events: roll back savepoint: %w", err)
	}

	n, err = archive(q)
	if err != nil {
		return false, translate(fmt.Errorf("events: archive: %w", err))
	}
	if n == 0 {
		return false, ErrNotFound
	}
	return true, tx.Commit(ctx)
}

// limit is how a question's ticket-type limit is stored: nil, never an empty
// array. An empty array would mean "asked of no ticket type", a question
// nobody sees, which is never what an unticked set of boxes meant.
func limit(q Question) []string {
	if q.Scope != ScopeAttendee || len(q.TicketTypeIDs) == 0 {
		return nil
	}
	return q.TicketTypeIDs
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func event(r gen.Event) Event {
	e := Event{
		ID: r.ID, Slug: r.Slug, Title: r.Title, Summary: r.Summary, Description: r.Description,
		Venue: r.Venue, Address: r.Address, StartsAt: r.StartsAt, EndsAt: r.EndsAt,
		Timezone: r.Timezone, Capacity: r.Capacity, Status: Status(r.Status), Listed: r.Listed,
		RegistrationOpensAt: r.RegistrationOpensAt, RegistrationClosesAt: r.RegistrationClosesAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.ImageKey != nil {
		e.ImageKey = *r.ImageKey
	}
	return e
}

func ticketType(r gen.TicketType) TicketType {
	return TicketType{
		ID: r.ID, EventID: r.EventID, Name: r.Name, Description: r.Description,
		PriceCents: r.PriceCents, Capacity: r.Capacity, MaxPerRegistration: r.MaxPerRegistration,
		SalesStartAt: r.SalesStartAt, SalesEndAt: r.SalesEndAt, Hidden: r.Hidden,
		Archived: r.Archived, Position: r.Position, CreatedAt: r.CreatedAt,
	}
}

func question(r gen.Question) Question {
	return Question{
		ID: r.ID, EventID: r.EventID, Scope: Scope(r.Scope), Kind: Kind(r.Kind),
		Label: r.Label, Help: r.Help, Options: r.Options, Required: r.Required,
		TicketTypeIDs: r.TicketTypeIDs, Position: r.Position, Archived: r.Archived,
		CreatedAt: r.CreatedAt,
	}
}

// translate maps driver errors to the domain's: no rows and a malformed UUID
// are both ErrNotFound — from outside, an id that could never exist and one
// that does not are the same answer — a foreign-key violation is ErrInUse, and
// the slug's unique violation is ErrSlugTaken for the form to put on the field.
func translate(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pgErr.Code {
		case "22P02":
			return ErrNotFound
		case "23503":
			return ErrInUse
		case "23505":
			if strings.Contains(pgErr.ConstraintName, "slug") {
				return ErrSlugTaken
			}
		}
	}
	return err
}
