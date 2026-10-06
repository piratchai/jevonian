import { BrowserRouter, Navigate, Route, Routes, useLocation } from "react-router";

import { Layout } from "@/components/layout";
import { ClientsPage } from "@/pages/clients";
import { KeysPage } from "@/pages/keys";
import { LogDetailPage } from "@/pages/log-detail";
import { LogsPage } from "@/pages/logs";
import { ModelsPage } from "@/pages/models";
import { OverviewPage } from "@/pages/overview";

function ModelsRedirect({ section }: { section: string }) {
  const { search, hash } = useLocation();
  return <Navigate to={`/models${search}${hash || `#${section}`}`} replace />;
}

export function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route element={<Layout />}>
          <Route index element={<OverviewPage />} />
          <Route path="models" element={<ModelsPage />} />
          <Route path="providers" element={<ModelsRedirect section="providers" />} />
          <Route path="clients" element={<ClientsPage />} />
          <Route path="routing" element={<ModelsRedirect section="task-routes" />} />
          <Route path="keys" element={<KeysPage />} />
          <Route path="activity" element={<Navigate to="/#activity" replace />} />
          <Route path="logs" element={<LogsPage />} />
          <Route path="logs/:id" element={<LogDetailPage />} />
        </Route>
      </Routes>
    </BrowserRouter>
  );
}
