import type { ReactNode } from 'react';
import { BrowserRouter, Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { AuthProvider, useAuth } from './auth/AuthContext';
import { Alert } from './components/Alert';
import { ErrorBoundary } from './components/ErrorBoundary';
import { Layout } from './components/Layout';
import { CamerasPage } from './pages/CamerasPage';
import { DashboardPage } from './pages/DashboardPage';
import { DestinationsPage } from './pages/DestinationsPage';
import { EventsPage } from './pages/EventsPage';
import { LivePage } from './pages/LivePage';
import { LoginPage } from './pages/LoginPage';
import { RecordingsPage } from './pages/RecordingsPage';
import { SystemPage } from './pages/SystemPage';

/** Where the login page sends the browser back to after signing in. */
export interface LoginLocationState {
  from?: string;
}

/** Shows its children only to a signed-in user; everyone else goes to /login. */
function RequireAuth({ children }: { children: ReactNode }) {
  const { status, error, refresh } = useAuth();
  const location = useLocation();

  if (status === 'loading') {
    return (
      <div className="splash" role="status">
        Loading…
      </div>
    );
  }
  if (status === 'failed') {
    return (
      <div className="splash">
        <Alert kind="error">{error ?? 'Could not check your session.'}</Alert>
        <button type="button" className="button button-primary" onClick={() => void refresh()}>
          Try again
        </button>
      </div>
    );
  }
  if (status === 'signedOut') {
    const state: LoginLocationState = { from: location.pathname + location.search };
    return <Navigate to="/login" replace state={state} />;
  }
  return <>{children}</>;
}

/** The System page (settings and users) is for admins only. */
function RequireAdmin({ children }: { children: ReactNode }) {
  const { isAdmin } = useAuth();
  return isAdmin ? <>{children}</> : <Navigate to="/" replace />;
}

export function App() {
  return (
    <ErrorBoundary>
      <BrowserRouter>
        <AuthProvider>
          <Routes>
            <Route path="/login" element={<LoginPage />} />
            <Route
              element={
                <RequireAuth>
                  <Layout />
                </RequireAuth>
              }
            >
              <Route index element={<DashboardPage />} />
              <Route path="live" element={<LivePage />} />
              <Route path="cameras" element={<CamerasPage />} />
              <Route path="destinations" element={<DestinationsPage />} />
              <Route path="recordings" element={<RecordingsPage />} />
              <Route path="events" element={<EventsPage />} />
              <Route
                path="system"
                element={
                  <RequireAdmin>
                    <SystemPage />
                  </RequireAdmin>
                }
              />
              <Route path="*" element={<Navigate to="/" replace />} />
            </Route>
          </Routes>
        </AuthProvider>
      </BrowserRouter>
    </ErrorBoundary>
  );
}
