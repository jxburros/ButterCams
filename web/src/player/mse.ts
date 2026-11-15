// Media Source Extensions helpers shared by the live player and the timeline
// player. iPhones have no MediaSource; from iOS 17.1 they have
// ManagedMediaSource, which works the same way but needs
// `disableRemotePlayback` on the video element and tells the page when to
// stop and resume appending (`endstreaming` / `startstreaming`).

type MediaSourceCtor = {
  new (): MediaSource;
  isTypeSupported(type: string): boolean;
};

interface MseGlobals {
  MediaSource?: MediaSourceCtor;
  ManagedMediaSource?: MediaSourceCtor;
}

function globals(): MseGlobals {
  return (typeof globalThis === 'undefined' ? {} : globalThis) as unknown as MseGlobals;
}

/** The constructor to use, or `null` when this browser has no MSE at all. */
export function mediaSourceCtor(): MediaSourceCtor | null {
  const g = globals();
  return g.MediaSource ?? g.ManagedMediaSource ?? null;
}

/** `true` when the browser only has ManagedMediaSource (iPhone). */
export function isManaged(): boolean {
  const g = globals();
  return !g.MediaSource && !!g.ManagedMediaSource;
}

/** `false` on browsers that cannot play live view or the timeline (iOS before 17.1). */
export function mseAvailable(): boolean {
  return mediaSourceCtor() !== null;
}

/** Whether the browser can play a full MIME type such as `video/mp4; codecs="avc1.64001f"`. */
export function isTypeSupported(mime: string): boolean {
  const ctor = mediaSourceCtor();
  if (!ctor) return false;
  try {
    return ctor.isTypeSupported(mime);
  } catch {
    return false;
  }
}

/** What to tell the user when `mseAvailable()` is false. */
export const NO_MSE_MESSAGE = 'Update iOS to 17.1 or later to watch.';

export interface AttachedSource {
  mediaSource: MediaSource;
  /** `true` when it is a ManagedMediaSource. */
  managed: boolean;
  /** Resolves when the source is open and SourceBuffers can be added. */
  opened: Promise<void>;
  /** Releases the object URL and detaches the source from the video. */
  detach: () => void;
}

/**
 * Creates a MediaSource (or ManagedMediaSource) and attaches it to a video
 * element. Returns `null` when the browser has no MSE.
 */
export function attachMediaSource(video: HTMLVideoElement): AttachedSource | null {
  const Ctor = mediaSourceCtor();
  if (!Ctor) return null;
  const managed = isManaged();
  const mediaSource = new Ctor();
  if (managed) {
    // Required by Safari for ManagedMediaSource.
    (video as HTMLVideoElement & { disableRemotePlayback: boolean }).disableRemotePlayback = true;
  }
  const url = URL.createObjectURL(mediaSource);
  const opened = new Promise<void>((resolve) => {
    if (mediaSource.readyState === 'open') resolve();
    else mediaSource.addEventListener('sourceopen', () => resolve(), { once: true });
  });
  video.src = url;
  let detached = false;
  return {
    mediaSource,
    managed,
    opened,
    detach: () => {
      if (detached) return;
      detached = true;
      URL.revokeObjectURL(url);
      if (video.src === url) {
        video.removeAttribute('src');
        try {
          video.load();
        } catch {
          // nothing to release
        }
      }
    },
  };
}
