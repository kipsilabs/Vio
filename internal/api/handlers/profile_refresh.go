package handlers

import "context"

// SignalsChangedNotifier records that a profile's recommendation signals
// (ratings, favorites, watchlist, watch progress or history) changed, so its
// taste profile and cached recommendations get rebuilt.
// *recommendations.Worker implements it.
type ProfileStaler interface {
	NotifySignalsChanged(context.Context, int, string)
}

type ProfileRefreshRequester interface {
	RequestProfileRefresh(context.Context, int, string)
}

type SignalsChangedNotifier interface {
	NotifySignalsChanged(ctx context.Context, userID int, profileID string)
}

func notifySignalsChanged(ctx context.Context, notifier SignalsChangedNotifier, userID int, profileID string) {
	if notifier != nil {
		notifier.NotifySignalsChanged(ctx, userID, profileID)
	}
}
