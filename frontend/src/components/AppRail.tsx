import { getJSON } from "../api/client";
import { appMark } from "../ky-ui/appmarks";

/** One suite app from `GET /api/suite/apps`: KyIdentity client ID, name and launch URL. */
export interface SuiteApp {
  id: string;
  name: string;
  url: string;
}

/** KyPost's own KyIdentity client ID: the rail marks it as the current app. */
const SELF = "kypost";

/** ky-ui's app rail; nothing when there are no apps (local accounts, no KyIdentity, a failure). */
export function AppRail({ apps }: { apps: SuiteApp[] }) {
  if (apps.length === 0) return null;
  return (
    <nav className="ky-app-rail" aria-label="Apps">
      <p className="ky-app-rail-label" aria-hidden="true">Apps</p>
      <ul>
        {apps.map((app) => (
          <li key={app.id}>
            <a href={app.url} title={app.name} aria-label={app.name} aria-current={app.id === SELF ? "page" : undefined}>
              <img src={appMark(app.id)} width={24} height={24} alt="" />
            </a>
          </li>
        ))}
      </ul>
    </nav>
  );
}

function isSuiteApp(v: unknown): v is SuiteApp {
  return typeof v === "object" && v !== null && "id" in v && "name" in v && "url" in v &&
    typeof v.id === "string" && v.id !== "" && typeof v.name === "string" && v.name !== "" &&
    typeof v.url === "string" && v.url.startsWith("https://");
}

/** The signed-in user's suite apps; [] on any failure or malformed answer. */
export async function loadSuiteApps(): Promise<SuiteApp[]> {
  try {
    const body = await getJSON<unknown>("/api/suite/apps");
    const apps = typeof body === "object" && body !== null && "apps" in body ? body.apps : undefined;
    return Array.isArray(apps) && apps.every(isSuiteApp) ? apps : [];
  } catch {
    return [];
  }
}
