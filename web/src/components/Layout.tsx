import { NavLink, Outlet } from 'react-router-dom';
import { useAuth } from '../auth/AuthContext';
import type { IconName } from './Icon';
import { BrandMark, Icon } from './Icon';

interface NavItem {
  to: string;
  label: string;
  icon: IconName;
  adminOnly?: boolean;
}

const NAV_ITEMS: NavItem[] = [
  { to: '/live', label: 'Live', icon: 'live' },
  { to: '/', label: 'Dashboard', icon: 'dashboard' },
  { to: '/cameras', label: 'Cameras', icon: 'cameras' },
  { to: '/destinations', label: 'Destinations', icon: 'destinations' },
  { to: '/recordings', label: 'Recordings', icon: 'recordings' },
  { to: '/events', label: 'Events', icon: 'events' },
  { to: '/system', label: 'System', icon: 'system', adminOnly: true },
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
        <NavLink to="/" end className="brand">
          <BrandMark />
          <span>ButterCams</span>
        </NavLink>
        <nav aria-label="Main">
          <ul className="nav-list">
            {items.map((item) => (
              <li key={item.to}>
                <NavLink to={item.to} end={item.to === '/'} className={({ isActive }) => (isActive ? 'nav-link nav-link-active' : 'nav-link')}>
                  <Icon name={item.icon} />
                  <span>{item.label}</span>
                </NavLink>
              </li>
            ))}
          </ul>
        </nav>
        {user ? (
          <div className="account">
            <div className="account-who">
              <div className="account-name">{user.username}</div>
              <div className="account-role">{user.role === 'admin' ? 'Admin' : 'Viewer'}</div>
            </div>
            <button type="button" className="account-signout" onClick={() => void logout()}>
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
