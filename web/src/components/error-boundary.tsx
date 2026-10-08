import { Component, type ErrorInfo, type ReactNode } from "react";

interface ErrorBoundaryState {
  error?: Error;
}

export class ErrorBoundary extends Component<{ children: ReactNode }, ErrorBoundaryState> {
  state: ErrorBoundaryState = {};

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error(error, info);
  }

  render() {
    if (this.state.error) {
      return (
        <div className="rounded-lg border border-kumo-danger/40 bg-kumo-danger-tint p-4 text-sm">
          <p className="font-medium text-kumo-danger">This page hit an error</p>
          <pre className="mt-2 overflow-auto text-xs whitespace-pre-wrap text-kumo-subtle">
            {this.state.error.message}
          </pre>
          <button
            type="button"
            className="mt-3 text-xs text-kumo-link underline"
            onClick={() => this.setState({ error: undefined })}
          >
            Retry
          </button>
        </div>
      );
    }
    return this.props.children;
  }
}
