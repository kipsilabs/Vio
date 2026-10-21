package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/jackc/pgx/v5/pgxpool"
)

// --- Stub repos for resolveParent ---

type stubEpisodeLookuper struct {
	ep  *models.Episode
	err error
}

func (s *stubEpisodeLookuper) GetByID(_ context.Context, _ string) (*models.Episode, error) {
	return s.ep, s.err
}

type stubSeasonLookuper struct {
	season *models.Season
	err    error
}

func (s *stubSeasonLookuper) GetByID(_ context.Context, _ string) (*models.Season, error) {
	return s.season, s.err
}

type stubItemLookuper struct {
	item *models.MediaItem
	err  error
}

func (s *stubItemLookuper) GetByID(_ context.Context, _ string) (*models.MediaItem, error) {
	return s.item, s.err
}

// --- parseRatingFlags tests ---

func TestParseRatingFlags(t *testing.T) {
	t.Run("valid rating", func(t *testing.T) {
		flags := parseRatingFlags([]byte(`{"rating":4}`))
		if flags.Rating != 4 {
			t.Fatalf("Rating = %d, want 4", flags.Rating)
		}
	})
	t.Run("empty input", func(t *testing.T) {
		flags := parseRatingFlags(nil)
		if flags.Rating != 0 {
			t.Fatalf("Rating = %d, want 0", flags.Rating)
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		flags := parseRatingFlags([]byte(`{bad json`))
		if flags.Rating != 0 {
			t.Fatalf("Rating = %d, want 0 for malformed JSON", flags.Rating)
		}
	})
}

// --- resolveParent tests ---

func TestResolveParent(t *testing.T) {
	ctx := context.Background()

	t.Run("episode", func(t *testing.T) {
		n := &RatingNotifier{
			episodeRepo: &stubEpisodeLookuper{
				ep: &models.Episode{ContentID: "episode-1", SeriesID: "series-1"},
			},
		}
		seriesID, episodeID := n.resolveParent(ctx, "episode-1")
		if seriesID == nil || *seriesID != "series-1" {
			t.Fatalf("seriesID = %v, want series-1", seriesID)
		}
		if episodeID == nil || *episodeID != "episode-1" {
			t.Fatalf("episodeID = %v, want episode-1", episodeID)
		}
	})

	t.Run("season", func(t *testing.T) {
		n := &RatingNotifier{
			episodeRepo: &stubEpisodeLookuper{err: errors.New("not found")},
			seasonRepo: &stubSeasonLookuper{
				season: &models.Season{SeriesID: "series-1"},
			},
		}
		seriesID, episodeID := n.resolveParent(ctx, "season-1")
		if seriesID == nil || *seriesID != "series-1" {
			t.Fatalf("seriesID = %v, want series-1", seriesID)
		}
		// C9 fix: season contentID is returned as episodeID so item_id
		// is the season, not the series.
		if episodeID == nil || *episodeID != "season-1" {
			t.Fatalf("episodeID = %v, want season-1 (the rated season)", episodeID)
		}
	})

	t.Run("movie/series", func(t *testing.T) {
		n := &RatingNotifier{
			episodeRepo: &stubEpisodeLookuper{err: errors.New("not found")},
			seasonRepo:  &stubSeasonLookuper{err: errors.New("not found")},
			itemRepo: &stubItemLookuper{
				item: &models.MediaItem{ContentID: "movie-1"},
			},
		}
		seriesID, episodeID := n.resolveParent(ctx, "movie-1")
		if seriesID == nil || *seriesID != "movie-1" {
			t.Fatalf("seriesID = %v, want movie-1", seriesID)
		}
		if episodeID != nil {
			t.Fatalf("episodeID = %v, want nil for movie/series", *episodeID)
		}
	})

	t.Run("unknown contentID falls back to contentID", func(t *testing.T) {
		n := &RatingNotifier{
			episodeRepo: &stubEpisodeLookuper{err: errors.New("not found")},
			seasonRepo:  &stubSeasonLookuper{err: errors.New("not found")},
			itemRepo:    &stubItemLookuper{err: errors.New("not found")},
		}
		seriesID, episodeID := n.resolveParent(ctx, "unknown-1")
		// C3 fix: unknown contentID falls back to using contentID as seriesID
		// so item_id is never empty.
		if seriesID == nil || *seriesID != "unknown-1" {
			t.Fatalf("seriesID = %v, want unknown-1 (C3 fallback)", seriesID)
		}
		if episodeID != nil {
			t.Fatalf("episodeID = %v, want nil for unknown content", *episodeID)
		}
	})
}

// --- BuildGenericWebhookPayload rating.set tests ---

func TestBuildGenericWebhookPayload_RatingSet(t *testing.T) {
	t.Run("episode item_id", func(t *testing.T) {
		epID := "episode-1"
		serID := "series-1"
		flags, _ := json.Marshal(RatingFlags{Rating: 5})
		row := DeliveryRow{
			Delivery: Delivery{
				Type:        DeliveryTypeRatingSet,
				SeriesID:    &serID,
				EpisodeID:   &epID,
				ReasonFlags: flags,
			},
		}
		body, err := BuildGenericWebhookPayload(row, "hook-1", false)
		if err != nil {
			t.Fatalf("BuildGenericWebhookPayload: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		rating, ok := payload["rating"].(map[string]any)
		if !ok {
			t.Fatal("payload missing rating object")
		}
		if rating["item_id"] != "episode-1" {
			t.Fatalf("item_id = %v, want episode-1", rating["item_id"])
		}
		if rating["rating"] != float64(5) {
			t.Fatalf("rating = %v, want 5", rating["rating"])
		}
	})

	t.Run("season item_id", func(t *testing.T) {
		serID := "series-1"
		// Season: episodeID carries the season's contentID (per C9 fix),
		// so item_id is the season, not the series.
		seasonID := "season-1"
		flags, _ := json.Marshal(RatingFlags{Rating: 3})
		row := DeliveryRow{
			Delivery: Delivery{
				Type:        DeliveryTypeRatingSet,
				SeriesID:    &serID,
				EpisodeID:   &seasonID,
				ReasonFlags: flags,
			},
		}
		body, err := BuildGenericWebhookPayload(row, "hook-1", false)
		if err != nil {
			t.Fatalf("BuildGenericWebhookPayload: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		rating, ok := payload["rating"].(map[string]any)
		if !ok {
			t.Fatal("payload missing rating object")
		}
		if rating["item_id"] != "season-1" {
			t.Fatalf("item_id = %v, want season-1", rating["item_id"])
		}
	})

	t.Run("movie item_id", func(t *testing.T) {
		serID := "movie-1"
		flags, _ := json.Marshal(RatingFlags{Rating: 2})
		row := DeliveryRow{
			Delivery: Delivery{
				Type:        DeliveryTypeRatingSet,
				SeriesID:    &serID,
				EpisodeID:   nil,
				ReasonFlags: flags,
			},
		}
		body, err := BuildGenericWebhookPayload(row, "hook-1", false)
		if err != nil {
			t.Fatalf("BuildGenericWebhookPayload: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		rating, ok := payload["rating"].(map[string]any)
		if !ok {
			t.Fatal("payload missing rating object")
		}
		if rating["item_id"] != "movie-1" {
			t.Fatalf("item_id = %v, want movie-1", rating["item_id"])
		}
	})
}

// --- NotifyRating nil/edge tests ---

func TestNotifyRatingNilGuards(t *testing.T) {
	ctx := context.Background()
	if err := (&RatingNotifier{}).NotifyRating(ctx, 1, "p", "c", 5); err != nil {
		t.Fatalf("nil notifier should return nil, got %v", err)
	}
	n := &RatingNotifier{system: &System{}}
	if err := n.NotifyRating(ctx, 0, "p", "c", 5); err != nil {
		t.Fatalf("userID <= 0 should return nil, got %v", err)
	}
	if err := n.NotifyRating(ctx, 1, "", "c", 5); err != nil {
		t.Fatalf("empty profileID should return nil, got %v", err)
	}
	if err := n.NotifyRating(ctx, 1, "p", "", 5); err != nil {
		t.Fatalf("empty contentID should return nil, got %v", err)
	}
}

// --- NotifyRating eligibility gate tests (DB-backed) ---
//
// The eligibility gate (C2) must match what DispatchOperational's post-commit
// enqueuers consider recipient-eligible: a webhook with notify_ratings, a
// web-push subscription, or an admin-enabled push platform with an enabled
// private-push device. These tests exercise the real repos against temp
// tables so the gate and the enqueuer share the same SQL.

// ratingNotifierTestDB connects to the test database, skipping when
// SILO_TEST_DATABASE_URL is unset (same convention as
// TestDispatchOperationalEnqueuesApplePushAttempts).
func ratingNotifierTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("SILO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SILO_TEST_DATABASE_URL to run DB-backed rating notifier eligibility tests")
	}
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse db config: %v", err)
	}
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect db: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ratingNotifierTestSchema creates the temp tables the eligibility gate and
// DispatchOperational touch, mirroring the real migrations' shapes.
func ratingNotifierTestSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		CREATE TEMP TABLE notification_deliveries (
			id text PRIMARY KEY,
			release_event_id text,
			user_id integer NOT NULL,
			profile_id text NOT NULL,
			library_id integer,
			series_id text,
			episode_id text,
			type text NOT NULL,
			reason_flags jsonb NOT NULL DEFAULT '{}'::jsonb,
			status text NOT NULL DEFAULT 'delivered',
			read_at timestamptz,
			delivered_at timestamptz,
			created_at timestamptz NOT NULL DEFAULT now()
		) ON COMMIT PRESERVE ROWS;

		CREATE TEMP TABLE notification_preferences (
			profile_id text PRIMARY KEY,
			user_id integer NOT NULL DEFAULT 1,
			enabled boolean NOT NULL DEFAULT true,
			notify_favorites boolean NOT NULL DEFAULT true,
			notify_watchlist boolean NOT NULL DEFAULT true,
			notify_continue_watching boolean NOT NULL DEFAULT true,
			notify_next_up boolean NOT NULL DEFAULT true,
			updated_at timestamptz NOT NULL DEFAULT now()
		) ON COMMIT PRESERVE ROWS;

		CREATE TEMP TABLE notification_webhooks (
			id text PRIMARY KEY,
			user_id integer NOT NULL,
			profile_id text NOT NULL,
			name varchar(64) NOT NULL,
			type text NOT NULL,
			url_ciphertext text NOT NULL,
			url_host varchar(253) NOT NULL,
			signing_secret_ciphertext text,
			enabled boolean NOT NULL DEFAULT true,
			notify_favorites boolean NOT NULL DEFAULT true,
			notify_watchlist boolean NOT NULL DEFAULT true,
			notify_continue_watching boolean NOT NULL DEFAULT true,
			notify_next_up boolean NOT NULL DEFAULT true,
			notify_requests boolean NOT NULL DEFAULT false,
			notify_ratings boolean NOT NULL DEFAULT false,
			consecutive_failures integer NOT NULL DEFAULT 0,
			disabled_reason varchar(256),
			last_success_at timestamptz,
			last_failure_at timestamptz,
			last_failure_status integer,
			last_failure_message varchar(256),
			created_at timestamptz NOT NULL DEFAULT now(),
			revision bigint NOT NULL DEFAULT 1,
			updated_at timestamptz NOT NULL DEFAULT now()
		) ON COMMIT PRESERVE ROWS;

		CREATE TEMP TABLE webhook_delivery_attempts (
			id text PRIMARY KEY,
			notification_delivery_id text NOT NULL,
			webhook_id text NOT NULL,
			attempt_number integer NOT NULL,
			attempted_at timestamptz NOT NULL DEFAULT now(),
			next_retry_at timestamptz,
			http_status integer,
			outcome text NOT NULL,
			failure_message varchar(256)
		) ON COMMIT PRESERVE ROWS;

		CREATE TEMP TABLE web_push_subscriptions (
			id text PRIMARY KEY,
			user_id integer NOT NULL,
			profile_id text NOT NULL,
			endpoint text NOT NULL,
			p256dh text NOT NULL,
			auth text NOT NULL,
			device_name varchar(128) NOT NULL DEFAULT '',
			enabled boolean NOT NULL DEFAULT true,
			consecutive_failures integer NOT NULL DEFAULT 0,
			last_success_at timestamptz,
			last_failure_at timestamptz,
			last_failure_status integer,
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now()
		) ON COMMIT PRESERVE ROWS;

		CREATE TEMP TABLE web_push_delivery_attempts (
			id text PRIMARY KEY,
			notification_delivery_id text NOT NULL,
			subscription_id text NOT NULL,
			attempt_number integer NOT NULL,
			attempted_at timestamptz NOT NULL DEFAULT now(),
			next_retry_at timestamptz,
			http_status integer,
			outcome text NOT NULL,
			failure_message varchar(256)
		) ON COMMIT PRESERVE ROWS;

		CREATE TEMP TABLE push_devices (
			id text PRIMARY KEY,
			user_id integer NOT NULL,
			profile_id text NOT NULL,
			device_id varchar(128) NOT NULL,
			platform text NOT NULL,
			provider text NOT NULL,
			apns_environment text,
			apns_topic text,
			apns_token_ciphertext text,
			apns_token_hash text,
			fcm_token_ciphertext text,
			fcm_token_hash text,
			server_device_id text NOT NULL,
			push_mode text NOT NULL DEFAULT 'private_push',
			enabled boolean NOT NULL DEFAULT true,
			last_seen_at timestamptz,
			last_success_at timestamptz,
			last_failure_at timestamptz,
			last_failure_code text,
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now()
		) ON COMMIT PRESERVE ROWS;

		CREATE TEMP TABLE push_delivery_attempts (
			id text PRIMARY KEY,
			notification_delivery_id text,
			push_device_id text NOT NULL,
			trigger_type text NOT NULL,
			provider text NOT NULL,
			platform text NOT NULL,
			attempt_number integer NOT NULL DEFAULT 0,
			attempted_at timestamptz,
			next_retry_at timestamptz,
			outcome text NOT NULL DEFAULT 'pending',
			relay_request_id text,
			upstream_status integer,
			upstream_reason text,
			failure_message text,
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now()
		) ON COMMIT PRESERVE ROWS;
	`); err != nil {
		t.Fatalf("create temp rating notifier tables: %v", err)
	}
}

// ratingNotifierTestSystem wires a System whose repos point at the temp
// tables. Settings enable webhooks, web push, and both native push platforms
// so each channel's gate is exercised; individual tests seed only the
// subscribers they want.
func ratingNotifierTestSystem(pool *pgxpool.Pool, libraryID int) *System {
	return &System{
		pool:           pool,
		Settings:       NewSettings(mapSettingReader{SettingWebhooksEnabled: "true", SettingApplePushDeliveryEnabled: "true", SettingAndroidPushDeliveryEnabled: "true"}),
		Deliveries:     NewDeliveryRepository(pool),
		Preferences:    NewPreferencesRepository(pool),
		webhookRepo:    NewWebhookRepository(pool),
		webPushRepo:    NewWebPushRepository(pool),
		pushDeviceRepo: NewPushDeviceRepository(pool),
		dispatcher:     NewMultiDispatcher(),
		scopes: scopeByProfile{
			"profile-native-only":    {},
			"profile-no-subscribers": {},
			"profile-webhook":        {},
			"profile-webpush":        {},
		},
		logger: slog.New(slog.DiscardHandler),
	}
}

// seedRatingWebhook inserts one webhook for the profile with the given
// notify_ratings flag.
func seedRatingWebhook(t *testing.T, pool *pgxpool.Pool, id, profileID string, notifyRatings bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO notification_webhooks
			(id, user_id, profile_id, name, type, url_ciphertext, url_host, signing_secret_ciphertext, notify_ratings)
		VALUES ($1, 42, $2, $3, 'generic', 'ciphertext', 'example.com', 'secret', $4)`,
		id, profileID, id, notifyRatings); err != nil {
		t.Fatalf("seed webhook: %v", err)
	}
}

// seedRatingWebPush inserts one enabled web-push subscription for the profile.
func seedRatingWebPush(t *testing.T, pool *pgxpool.Pool, id, profileID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO web_push_subscriptions (id, user_id, profile_id, endpoint, p256dh, auth)
		VALUES ($1, 42, $2, $3, 'p256dh', 'auth')`,
		id, profileID, "https://push.example.com/"+id); err != nil {
		t.Fatalf("seed web push subscription: %v", err)
	}
}

// seedRatingPushDevice inserts one native push device for the profile with
// the given platform, push_mode, and enabled flag.
func seedRatingPushDevice(t *testing.T, pool *pgxpool.Pool, id, profileID, platform, pushMode string, enabled bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO push_devices
			(id, user_id, profile_id, device_id, platform, provider, server_device_id, push_mode, enabled,
			 apns_environment, apns_topic, apns_token_ciphertext, apns_token_hash,
			 fcm_token_ciphertext, fcm_token_hash)
		VALUES ($1, 42, $2, $3, $4, 'silo_relay', $5, $6, $7, $8, $8, $8, $8, $8, $8)`,
		id, profileID, "device-"+id, platform, "server-"+id, pushMode, enabled,
		"ciphertext"); err != nil {
		t.Fatalf("seed push device: %v", err)
	}
}

// countRatingDeliveries returns the number of rating.set delivery rows for
// the profile.
func countRatingDeliveries(t *testing.T, pool *pgxpool.Pool, profileID string) int {
	t.Helper()
	ctx := context.Background()
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notification_deliveries WHERE profile_id = $1 AND type = 'rating.set'`,
		profileID).Scan(&count); err != nil {
		t.Fatalf("count rating deliveries: %v", err)
	}
	return count
}

// TestNotifyRatingEligibilityGate exercises the C2 gate against the real
// repos: a delivery row is written only when at least one channel would
// actually carry it.
func TestNotifyRatingEligibilityGate(t *testing.T) {
	pool := ratingNotifierTestDB(t)
	ratingNotifierTestSchema(t, pool)
	c := seedAccessCatalog(t, pool)
	ctx := context.Background()

	t.Run("native-only subscribers pass the gate and create a delivery", func(t *testing.T) {
		profileID := "profile-native-only"
		seedRatingPushDevice(t, pool, "apple-1", profileID, PushPlatformApple, PushModePrivatePush, true)
		seedRatingPushDevice(t, pool, "android-1", profileID, PushPlatformAndroid, PushModePrivatePush, true)

		system := ratingNotifierTestSystem(pool, c.library)
		notifier := &RatingNotifier{system: system, itemRepo: &stubItemLookuper{item: &models.MediaItem{ContentID: c.series}}}
		if err := notifier.NotifyRating(ctx, 42, profileID, "movie-1", 5); err != nil {
			t.Fatalf("NotifyRating: %v", err)
		}
		if got := countRatingDeliveries(t, pool, profileID); got != 1 {
			t.Fatalf("rating.set deliveries = %d, want 1 (native-only profile must not be gated out)", got)
		}
	})

	t.Run("no eligible subscribers of any kind exits without a delivery", func(t *testing.T) {
		profileID := "profile-no-subscribers"
		// A webhook exists but has notify_ratings off; a push device exists
		// but is disabled — neither is recipient-eligible.
		seedRatingWebhook(t, pool, "hook-off", profileID, false)
		seedRatingPushDevice(t, pool, "apple-disabled", profileID, PushPlatformApple, PushModePrivatePush, false)

		system := ratingNotifierTestSystem(pool, c.library)
		notifier := &RatingNotifier{system: system, itemRepo: &stubItemLookuper{item: &models.MediaItem{ContentID: c.series}}}
		if err := notifier.NotifyRating(ctx, 42, profileID, "movie-1", 5); err != nil {
			t.Fatalf("NotifyRating: %v", err)
		}
		if got := countRatingDeliveries(t, pool, profileID); got != 0 {
			t.Fatalf("rating.set deliveries = %d, want 0 (no eligible subscribers)", got)
		}
	})

	t.Run("webhook with notify_ratings still passes the gate", func(t *testing.T) {
		profileID := "profile-webhook"
		seedRatingWebhook(t, pool, "hook-on", profileID, true)

		system := ratingNotifierTestSystem(pool, c.library)
		notifier := &RatingNotifier{system: system, itemRepo: &stubItemLookuper{item: &models.MediaItem{ContentID: c.series}}}
		if err := notifier.NotifyRating(ctx, 42, profileID, "movie-1", 5); err != nil {
			t.Fatalf("NotifyRating: %v", err)
		}
		if got := countRatingDeliveries(t, pool, profileID); got != 1 {
			t.Fatalf("rating.set deliveries = %d, want 1 (webhook with notify_ratings)", got)
		}
	})

	t.Run("web push subscription still passes the gate", func(t *testing.T) {
		profileID := "profile-webpush"
		seedRatingWebPush(t, pool, "sub-1", profileID)

		system := ratingNotifierTestSystem(pool, c.library)
		notifier := &RatingNotifier{system: system, itemRepo: &stubItemLookuper{item: &models.MediaItem{ContentID: c.series}}}
		if err := notifier.NotifyRating(ctx, 42, profileID, "movie-1", 5); err != nil {
			t.Fatalf("NotifyRating: %v", err)
		}
		if got := countRatingDeliveries(t, pool, profileID); got != 1 {
			t.Fatalf("rating.set deliveries = %d, want 1 (web push subscription)", got)
		}
	})
}
