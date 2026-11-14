import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
// The fonts are bundled into the page (and so into buttercams.exe): the UI
// must work on a network with no internet access.
import '@fontsource/bricolage-grotesque/latin-500.css';
import '@fontsource/bricolage-grotesque/latin-800.css';
import '@fontsource/bricolage-grotesque/latin-ext-800.css';
import '@fontsource/instrument-sans/latin-400.css';
import '@fontsource/instrument-sans/latin-ext-400.css';
import '@fontsource/instrument-sans/latin-500.css';
import '@fontsource/instrument-sans/latin-600.css';
import '@fontsource/instrument-sans/latin-ext-600.css';
import '@fontsource/instrument-sans/latin-700.css';
import '@fontsource/martian-mono/latin-400.css';
import '@fontsource/martian-mono/latin-600.css';
import { App } from './App';
import './styles.css';

const root = document.getElementById('root');
if (!root) throw new Error('index.html is missing the #root element.');

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
