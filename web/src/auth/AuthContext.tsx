import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { ApiError, api, errorMessage, setCsrfToken, setUnauthorizedHandler } from '../api/client';
import type { Session, User } from '../api/types';

type AuthState =
  | { status: 'loading' }
  | { status: 'signedOut' }
  | { status: 'signedIn'; session: Session }
  | { status: 'failed'; message: string };

export interface AuthContextValue {
  /** `loading` until GET /auth/session has answered. */
  status: AuthState['status'];
  user: User | null;
  csrfToken: string | null;
  /** `true` for the admin role; viewers get read-only pages. */
  isAdmin: boolean;
  /** Set when the session check failed for a reason other than "not signed in". */
  error: string | null;
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  /** Asks the server for the session again (after the user's own role changes). */
  refresh: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>({ status: 'loading' });

  const signedIn = useCallback((session: Session) => {
    setCsrfToken(session.csrfToken);
    setState({ status: 'signedIn', session });
  }, []);

  const signedOut = useCallback(() => {
    setCsrfToken('');
    setState({ status: 'signedOut' });
  }, []);

  const refresh = useCallback(async () => {
    try {
      signedIn(await api.session());
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        signedOut();
      } else {
        setState({ status: 'failed', message: errorMessage(err) });
      }
    }
  }, [signedIn, signedOut]);

  useEffect(() => {
    // Any 401 from the API means the session is gone: drop it, and the route
    // guard sends the browser to the login page.
    setUnauthorizedHandler(signedOut);
    void refresh();
    return () => setUnauthorizedHandler(null);
  }, [refresh, signedOut]);

  const login = useCallback(
    async (username: string, password: string) => {
      signedIn(await api.login({ username, password }));
    },
    [signedIn],
  );

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } catch {
      // The session is dropped on this side either way.
    }
    signedOut();
  }, [signedOut]);

  const value = useMemo<AuthContextValue>(() => {
    const session = state.status === 'signedIn' ? state.session : null;
    return {
      status: state.status,
      user: session?.user ?? null,
      csrfToken: session?.csrfToken ?? null,
      isAdmin: session?.user.role === 'admin',
      error: state.status === 'failed' ? state.message : null,
      login,
      logout,
      refresh,
    };
  }, [state, login, logout, refresh]);

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const value = useContext(AuthContext);
  if (value === null) throw new Error('useAuth must be used inside <AuthProvider>.');
  return value;
}
