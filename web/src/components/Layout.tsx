import { NavLink, Outlet } from 'react-router-dom';
import { useAuth } from '../auth/AuthContext';

interface NavItem {
  to: string;
  label: string;
  adminOnly?: boolean;
}

const NAV_ITEMS: NavItem[] = [
  { to: '/', label: 'Dashboard' },
  { to: '/cameras', label: 'Cameras' },
  { to: '/destinations', label: 'Destinations' },
  { to: '/recordings', label: 'Recordings' },
  { to: '/events', label: 'Events' },
  { to: '/system', label: 'System', adminOnly: true },
];

/** The page frame: navigation on the left, the current page on the right. */
export function Layout() {
  const { user, isAdmin, logout } = useAuth();
  const items = NAV_ITEMS.filter((item) => isAdmin || !item.adminOnly);

  return (
    <div className="app">
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      <header className="sidebar">
        <div className="brand">ButterCams</div>
        <nav aria-label="Main">
          <ul className="nav-list">
            {items.map((item) => (
              <li key={item.to}>
                <NavLink to={item.to} end={item.to === '/'} className={({ isActive }) => (isActive ? 'nav-link nav-link-active' : 'nav-link')}>
                  {item.label}
                </NavLink>
              </li>
            ))}
          </ul>
        </nav>
        {user ? (
          <div className="account">
            <div className="account-name">{user.username}</div>
            <div className="account-role">{user.role === 'admin' ? 'Admin' : 'Viewer'}</div>
            <button type="button" className="button button-small" onClick={() => void logout()}>
              Sign out
            </button>
          </div>
        ) : null}
      </header>
      <main id="main" className="content" tabIndex={-1}>
        <Outlet />
      </main>
    </div>
  );
}
