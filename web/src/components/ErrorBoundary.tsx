import { Component } from 'react';
import type { ErrorInfo, ReactNode } from 'react';

interface Props {
  children: ReactNode;
}

interface State {
  failed: boolean;
}

/** Shows a way out instead of a blank page if a screen fails to draw. */
export class ErrorBoundary extends Component<Props, State> {
  override state: State = { failed: false };

  static getDerivedStateFromError(): State {
    return { failed: true };
  }

  override componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error('ButterCams page error:', error, info.componentStack);
  }

  override render(): ReactNode {
    if (!this.state.failed) return this.props.children;
    return (
      <div className="splash">
        <div className="alert alert-error" role="alert">
          This page could not be shown. Recording is not affected. Reload the page to try again.
        </div>
        <button type="button" className="button button-primary" onClick={() => window.location.reload()}>
          Reload
        </button>
      </div>
    );
  }
}
