import { createEffect } from "solid-js";
import type { JSX } from "solid-js";
import type { RouteSectionProps } from "@solidjs/router";
import { I18nProvider, useI18n } from "./hooks/useLocale";
import { CustomizeProvider } from "./hooks/useCustomize";
import { AuthProvider, useAuth } from "./hooks/useAuth";
import "leaflet/dist/leaflet.css";
import "maplibre-gl/dist/maplibre-gl.css";
import "./styles/variables.css";
import "./styles/base.css";
import "./styles/global.css";
import "./styles/entities.css";
import "./styles/leaflet.css";
import "./styles/responsive.css";

/**
 * Root layout component.
 *
 * Provides i18n, customize, and auth contexts around routed page content.
 */
export function App(props: RouteSectionProps): JSX.Element {
  return (
    <I18nProvider>
      <CustomizeProvider>
        <AuthProvider>
          <SuiteLocale />
          {props.children}
        </AuthProvider>
      </CustomizeProvider>
    </I18nProvider>
  );
}

/**
 * With the TLB suite sign-in ("tlb" auth mode) OCAP is English only, like the
 * other TLB sites: its language pickers are hidden, so a saved or browser
 * language is replaced by English.
 */
function SuiteLocale(): null {
  const { authMode } = useAuth();
  const { locale, setLocale } = useI18n();
  createEffect(() => {
    if (authMode() === "tlb" && locale() !== "en") setLocale("en");
  });
  return null;
}
