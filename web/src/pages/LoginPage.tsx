import { useState } from 'react';
import type { FormEvent } from 'react';
import { Navigate, useLocation } from 'react-router-dom';
import type { LoginLocationState } from '../App';
import { ApiError, errorMessage } from '../api/client';
import { useAuth } from '../auth/AuthContext';
import { Alert } from '../components/Alert';
import { TextField } from '../components/Fields';
import type { FieldErrors } from '../lib/forms';
import { hasErrors } from '../lib/forms';
import { usePageTitle } from '../lib/hooks';

function loginErrorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    // The server's own wording comes first; these are only fallbacks.
    if (err.message !== '') return err.message;
    if (err.code === 'invalid_credentials') return 'The user name or password is not correct.';
    if (err.code === 'account_locked') {
      return 'This account is locked after too many failed sign-ins. Try again in 15 minutes.';
    }
  }
  return errorMessage(err);
}

export function LoginPage() {
  usePageTitle('Sign in');
  const { status, login } = useAuth();
  const location = useLocation();

  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [errors, setErrors] = useState<FieldErrors>({});
  const [failure, setFailure] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (status === 'signedIn') {
    const from = (location.state as LoginLocationState | null)?.from;
    const target = from && from.startsWith('/') && !from.startsWith('//') && !from.startsWith('/login') ? from : '/';
    return <Navigate to={target} replace />;
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    const found: FieldErrors = {};
    if (username.trim() === '') found.username = 'Enter your user name.';
    if (password === '') found.password = 'Enter your password.';
    setErrors(found);
    setFailure(null);
    if (hasErrors(found)) return;

    setBusy(true);
    try {
      await login(username.trim(), password);
    } catch (err) {
      setFailure(loginErrorMessage(err));
      setBusy(false);
    }
  }

  return (
    <main className="login">
      <form className="login-card" onSubmit={(event) => void submit(event)} noValidate>
        <h1>ButterCams</h1>
        <p className="muted">Sign in to see recorder status and settings.</p>
        {failure ? <Alert kind="error">{failure}</Alert> : null}
        <TextField
          label="User name"
          value={username}
          onChange={setUsername}
          error={errors.username}
          autoComplete="username"
          autoCapitalize="none"
          spellCheck={false}
          autoFocus
        />
        <TextField
          label="Password"
          type="password"
          value={password}
          onChange={setPassword}
          error={errors.password}
          autoComplete="current-password"
        />
        <button type="submit" className="button button-primary" disabled={busy || status === 'loading'}>
          {busy ? 'Signing in…' : 'Sign in'}
        </button>
      </form>
    </main>
  );
}
