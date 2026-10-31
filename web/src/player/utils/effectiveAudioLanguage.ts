interface AudioLanguageTrack {
  language?: string;
}

interface AudioLanguageVersion {
  audio_tracks?: AudioLanguageTrack[];
  effective_audio_track_index?: number;
  effective_audio_language?: string;
}

function normalizeLanguage(language: string | null | undefined): string | null {
  const normalized = language?.trim();
  return normalized ? normalized : null;
}

export function resolveVersionAudioLanguage<T extends AudioLanguageVersion>(
  version: T | null | undefined,
  trackIndex: number | null | undefined,
): string | null {
  // A negative index is the "no applied track" marker
  // `resolveAppliedAudioIndex` returns when the committed selection has no row
  // in the applied inventory. It names no track, so it must not match a version
  // whose own effective index is also negative and resurrect that language.
  const index = trackIndex != null && trackIndex >= 0 ? trackIndex : null;
  const trackLanguage =
    index != null ? normalizeLanguage(version?.audio_tracks?.[index]?.language) : null;
  if (trackLanguage) {
    return trackLanguage;
  }

  const effectiveTrackIndex = version?.effective_audio_track_index;
  if (
    index == null ||
    effectiveTrackIndex == null ||
    effectiveTrackIndex < 0 ||
    index !== effectiveTrackIndex
  ) {
    return null;
  }

  return normalizeLanguage(version?.effective_audio_language);
}
