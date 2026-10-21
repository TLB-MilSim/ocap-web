import { createContext, useContext, createSignal, createMemo, onMount } from "solid-js";
import type { JSX, Accessor } from "solid-js";
import { ApiClient, getAuthToken, setAuthToken } from "../data/apiClient";

/** TLB suite top bar (public/tlb/tlb-suite-bar.js), present on every page. */
declare global {
  interface Window { TLBSuite?: { authUrl: (path: string, returnTo?: string) => string } }
}

/** The TLB sign-in sets a display cookie next to its session cookie; gone = signed out of the suite. */
const tlbSignedIn = () => /(?:^|;\s*)tlb_user=/.test(document.cookie);

export interface Auth {
  authenticated: Accessor<boolean>;
  role: Accessor<string | null>;
  isAdmin: Accessor<boolean>;
  steamId: Accessor<string | null>;
  steamName: Accessor<string | null>;
  steamAvatar: Accessor<string | null>;
  authError: Accessor<string | null>;
  authMode: Accessor<string>;
  dismissAuthError: () => void;
  loginWithSteam: () => void;
  loginWithPassword: (password: string) => Promise<void>;
  logout: () => Promise<void>;
}

const AUTH_ERROR_MESSAGES: Record<string, string> = {
  steam_error: "Steam login failed. Please try again.",
  not_allowed: "You are not on the allowlist. Contact an admin for access.",
};

const AuthContext = createContext<Auth>();

/**
 * Provider that checks session state on mount and exposes Steam login/logout actions app-wide.
 */
export function AuthProvider(props: { children: JSX.Element }): JSX.Element {
  const [authenticated, setAuthenticated] = createSignal(false);
  const [role, setRole] = createSignal<string | null>(null);
  const isAdmin = createMemo(() => role() === "admin");
  const [steamId, setSteamId] = createSignal<string | null>(null);
  const [steamName, setSteamName] = createSignal<string | null>(null);
  const [steamAvatar, setSteamAvatar] = createSignal<string | null>(null);
  const [authError, setAuthError] = createSignal<string | null>(null);
  const [authMode, setAuthMode] = createSignal<string>("public");
  const api = new ApiClient();

  onMount(async () => {
    // Fetch auth config first
    try {
      const config = await api.getAuthConfig();
      setAuthMode(config.mode);
    } catch {
      // Default to public if endpoint fails
    }

    // Read query params from Steam callback redirect
    const params = new URLSearchParams(window.location.search);
    // The TLB sign-in returns the token in the URL fragment, which browsers never send to servers.
    const hashToken = new URLSearchParams(window.location.hash.slice(1)).get("auth_token");
    if (hashToken) params.set("auth_token", hashToken);

    const error = params.get("auth_error");
    if (error) {
      setAuthError(AUTH_ERROR_MESSAGES[error] ?? "Authentication failed.");
    }

    const hadToken = api.consumeAuthToken(params);

    // Clean auth params from URL and restore pre-login path
    if (params.has("auth_error") || params.has("auth_token")) {
      params.delete("auth_error");
      params.delete("auth_token");
      const returnTo = hadToken ? api.popReturnTo() : null;
      if (returnTo && returnTo !== "/") {
        // replaceState + popstate triggers the SolidJS router to re-evaluate
        window.history.replaceState({}, "", returnTo);
        window.dispatchEvent(new PopStateEvent("popstate"));
      } else {
        const qs = params.toString();
        window.history.replaceState({}, "", window.location.pathname + (qs ? "?" + qs : ""));
      }
    }

    // TLB suite sign-in: no OCAP login screen. Without a token, or after signing out of the
    // suite elsewhere, go through /api/v1/auth/tlb, which signs in via the TLB sign-in if needed.
    if (authMode() === "tlb" && !error) {
      if (!hadToken && !tlbSignedIn()) setAuthToken(null);
      if (!getAuthToken()) {
        window.location.href = api.getTlbLoginUrl(window.location.pathname + window.location.search);
        return;
      }
    }

    if (!getAuthToken()) {
      setAuthenticated(false);
      return;
    }
    try {
      const state = await api.getMe();
      if (!state.authenticated && authMode() === "tlb" && !hadToken && !error) {
        // The TLB session ended or changed (signed out, blocked, role removed): sign in again.
        setAuthToken(null);
        window.location.href = api.getTlbLoginUrl(window.location.pathname + window.location.search);
        return;
      }
      setAuthenticated(state.authenticated);
      setRole(state.role ?? null);
      setSteamId(state.steamId ?? null);
      setSteamName(state.steamName ?? null);
      setSteamAvatar(state.steamAvatar ?? null);
    } catch {
      setAuthenticated(false);
    }
  });

  const dismissAuthError = () => setAuthError(null);

  const loginWithPassword = async (password: string): Promise<void> => {
    setAuthError(null);
    try {
      await api.passwordLogin(password);
      const state = await api.getMe();
      setAuthenticated(state.authenticated);
      setRole(state.role ?? null);
      setSteamId(state.steamId ?? null);
      setSteamName(state.steamName ?? null);
      setSteamAvatar(state.steamAvatar ?? null);
    } catch (err) {
      setAuthError(err instanceof Error ? err.message : "Login failed");
    }
  };

  const loginWithSteam = () => {
    setAuthError(null);
    if (authMode() === "tlb") {
      window.location.href = api.getTlbLoginUrl(window.location.pathname + window.location.search);
      return;
    }
    window.location.href = api.getSteamLoginUrl(
      window.location.pathname + window.location.search,
    );
  };

  const logout = async (): Promise<void> => {
    try {
      await api.logout();
    } finally {
      setAuthenticated(false);
      setRole(null);
      setSteamId(null);
      setSteamName(null);
      setSteamAvatar(null);
    }
    // TLB suite: sign out of the whole suite, not just OCAP.
    if (authMode() === "tlb" && window.TLBSuite) {
      window.location.href = window.TLBSuite.authUrl("logout", window.location.origin + "/");
    }
  };

  return (
    <AuthContext.Provider value={{ authenticated, role, isAdmin, steamId, steamName, steamAvatar, authError, authMode, dismissAuthError, loginWithSteam, loginWithPassword, logout }}>
      {props.children}
    </AuthContext.Provider>
  );
}

/**
 * Hook to access auth state and actions from any component within the AuthProvider.
 */
export function useAuth(): Auth {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error("useAuth must be used within an AuthProvider");
  }
  return ctx;
}
