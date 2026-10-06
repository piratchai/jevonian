import { useParams } from "react-router";

import { LogDetailView } from "@/components/log-detail/log-detail-view";

export function LogDetailPage() {
  const { id = "" } = useParams();
  return <LogDetailView id={id} variant="page" />;
}
