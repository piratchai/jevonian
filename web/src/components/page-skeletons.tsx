import { LayerCard, SkeletonLine } from "@cloudflare/kumo";

import { LOG_COLUMNS } from "@/components/logs/columns";
import { cn } from "@/lib/utils";

export function PageHeaderSkeleton({ descriptionWidth = "w-96" }: { descriptionWidth?: string }) {
  return (
    <div className="flex flex-col gap-2">
      <SkeletonLine blockHeight={24} className="w-28" />
      <SkeletonLine blockHeight={16} className={cn("max-w-full", descriptionWidth)} />
    </div>
  );
}

export function StatCardsSkeleton({
  count = 4,
  columns = "grid-cols-2 lg:grid-cols-4",
}: {
  count?: number;
  columns?: string;
}) {
  return (
    <div className={cn("grid gap-4", columns)}>
      {Array.from({ length: count }, (_, index) => (
        <LayerCard key={index}>
          <LayerCard.Secondary className="block">
            <div className="flex flex-col gap-2">
              <SkeletonLine blockHeight={12} className="w-20" />
              <SkeletonLine blockHeight={28} className="w-24" />
            </div>
          </LayerCard.Secondary>
          <LayerCard.Primary>
            <SkeletonLine blockHeight={12} className="w-32" />
          </LayerCard.Primary>
        </LayerCard>
      ))}
    </div>
  );
}

export function TableRowsSkeleton({ rows = 5, columns = 6 }: { rows?: number; columns?: number }) {
  return (
    <div className="flex flex-col gap-2">
      <div className="flex gap-3 border-b border-kumo-hairline pb-2">
        {Array.from({ length: columns }, (_, index) => (
          <SkeletonLine key={index} className="h-3 flex-1" />
        ))}
      </div>
      {Array.from({ length: rows }, (_, row) => (
        <div key={row} className="flex gap-3 py-2">
          {Array.from({ length: columns }, (_, col) => (
            <SkeletonLine
              key={col}
              className={cn("h-4 flex-1", col === 0 ? "max-w-[30%]" : undefined)}
            />
          ))}
        </div>
      ))}
    </div>
  );
}

export function CardBlockSkeleton({
  lines = 4,
  className,
}: {
  lines?: number;
  className?: string;
}) {
  return (
    <LayerCard className={className}>
      <LayerCard.Secondary className="block">
        <div className="flex flex-col gap-2">
          <SkeletonLine blockHeight={16} className="w-40" />
          <SkeletonLine blockHeight={12} className="w-64 max-w-full" />
        </div>
      </LayerCard.Secondary>
      <LayerCard.Primary className="flex flex-col gap-2">
        {Array.from({ length: lines }, (_, index) => (
          <SkeletonLine
            key={index}
            blockHeight={16}
            className={index % 2 === 0 ? "w-full" : "w-[80%]"}
          />
        ))}
      </LayerCard.Primary>
    </LayerCard>
  );
}

export function OverviewSkeleton() {
  return (
    <div className="flex flex-col gap-6" aria-busy="true" aria-label="Loading overview">
      <div className="flex flex-col gap-3">
        <SkeletonLine blockHeight={32} className="w-48" />
        <SkeletonLine blockHeight={16} className="w-64" />
        <div className="h-11 w-full animate-pulse rounded-xl bg-kumo-fill" />
      </div>
      <div className="grid grid-cols-1 gap-4 xl:grid-cols-12">
        <div className="flex flex-col gap-4 xl:col-span-7">
          <div className="h-72 w-full animate-pulse rounded-xl bg-kumo-fill" />
          <div className="h-64 w-full animate-pulse rounded-xl bg-kumo-fill" />
        </div>
        <div className="flex flex-col gap-4 xl:col-span-5">
          <div className="h-40 w-full animate-pulse rounded-xl bg-kumo-fill" />
          <div className="h-36 w-full animate-pulse rounded-xl bg-kumo-fill" />
          <div className="h-36 w-full animate-pulse rounded-xl bg-kumo-fill" />
        </div>
      </div>
      <CardBlockSkeleton lines={2} />
    </div>
  );
}

export function ProvidersSkeleton() {
  return (
    <div className="flex flex-col gap-6" aria-busy="true" aria-label="Loading providers">
      <PageHeaderSkeleton descriptionWidth="w-[32rem]" />
      <LayerCard>
        <LayerCard.Secondary className="flex-row items-start justify-between gap-4">
          <div className="flex flex-col gap-2">
            <SkeletonLine blockHeight={16} className="w-40" />
            <SkeletonLine blockHeight={12} className="w-24" />
          </div>
          <div className="h-8 w-28 animate-pulse rounded-md bg-kumo-fill" />
        </LayerCard.Secondary>
        <LayerCard.Primary>
          <TableRowsSkeleton rows={4} columns={6} />
        </LayerCard.Primary>
      </LayerCard>
      <div className="flex flex-col gap-3">
        <SkeletonLine blockHeight={16} className="w-32" />
        <SkeletonLine blockHeight={12} className="w-72 max-w-full" />
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {Array.from({ length: 3 }, (_, index) => (
            <div key={index} className="h-28 w-full animate-pulse rounded-lg bg-kumo-fill" />
          ))}
        </div>
      </div>
    </div>
  );
}

export function KeysSkeleton() {
  return (
    <div className="flex flex-col gap-6" aria-busy="true" aria-label="Loading keys">
      <PageHeaderSkeleton descriptionWidth="w-full max-w-2xl" />
      <CardBlockSkeleton lines={3} />
      <LayerCard>
        <LayerCard.Secondary className="block">
          <div className="flex flex-col gap-2">
            <SkeletonLine blockHeight={16} className="w-28" />
            <SkeletonLine blockHeight={12} className="w-56" />
          </div>
        </LayerCard.Secondary>
        <LayerCard.Primary>
          <TableRowsSkeleton rows={4} columns={7} />
        </LayerCard.Primary>
      </LayerCard>
    </div>
  );
}

export function ClientsSkeleton() {
  return (
    <div
      className="mx-auto flex max-w-4xl flex-col gap-6"
      aria-busy="true"
      aria-label="Loading clients"
    >
      <div className="flex items-start justify-between gap-4">
        <PageHeaderSkeleton descriptionWidth="w-80" />
        <div className="h-8 w-24 shrink-0 animate-pulse rounded-md bg-kumo-fill" />
      </div>
      <div className="h-10 w-full animate-pulse rounded-md bg-kumo-fill" />
      <div className="flex flex-col gap-4">
        {Array.from({ length: 2 }, (_, index) => (
          <LayerCard key={index}>
            <LayerCard.Secondary className="block">
              <div className="flex items-center justify-between gap-3">
                <div className="flex items-center gap-3">
                  <div className="size-6 animate-pulse rounded-md bg-kumo-fill" />
                  <div className="flex flex-col gap-2">
                    <SkeletonLine blockHeight={16} className="w-32" />
                    <SkeletonLine blockHeight={12} className="w-48" />
                  </div>
                </div>
                <div className="h-5 w-24 animate-pulse rounded-full bg-kumo-fill" />
              </div>
            </LayerCard.Secondary>
            <LayerCard.Primary className="flex flex-col gap-3">
              <SkeletonLine blockHeight={16} className="w-full" />
              <div className="flex gap-2">
                <div className="h-8 w-24 animate-pulse rounded-md bg-kumo-fill" />
                <div className="h-8 w-24 animate-pulse rounded-md bg-kumo-fill" />
              </div>
            </LayerCard.Primary>
          </LayerCard>
        ))}
      </div>
    </div>
  );
}

export function RoutingSkeleton() {
  return (
    <div className="flex flex-col gap-6" aria-busy="true" aria-label="Loading routing">
      <PageHeaderSkeleton descriptionWidth="w-[36rem]" />
      <LayerCard>
        <LayerCard.Secondary className="flex-row items-start justify-between gap-4">
          <div className="flex flex-col gap-2">
            <SkeletonLine blockHeight={16} className="w-32" />
            <SkeletonLine blockHeight={12} className="w-80 max-w-full" />
          </div>
          <div className="h-8 w-28 animate-pulse rounded-md bg-kumo-fill" />
        </LayerCard.Secondary>
        <LayerCard.Primary className="grid gap-4 md:grid-cols-2">
          {Array.from({ length: 4 }, (_, index) => (
            <div
              key={index}
              className="flex flex-col gap-3 rounded-lg border border-kumo-hairline bg-kumo-elevated p-4"
            >
              <SkeletonLine blockHeight={16} className="w-24" />
              <SkeletonLine blockHeight={12} className="w-full" />
              <SkeletonLine blockHeight={12} className="w-[75%]" />
              <div className="flex flex-wrap gap-2 pt-1">
                <div className="h-6 w-20 animate-pulse rounded-full bg-kumo-fill" />
                <div className="h-6 w-24 animate-pulse rounded-full bg-kumo-fill" />
                <div className="h-6 w-16 animate-pulse rounded-full bg-kumo-fill" />
              </div>
            </div>
          ))}
        </LayerCard.Primary>
      </LayerCard>
    </div>
  );
}

export function ActivitySkeleton() {
  return (
    <div className="flex flex-col gap-6" aria-busy="true" aria-label="Loading activity">
      <div className="flex flex-wrap items-center justify-between gap-4">
        <PageHeaderSkeleton descriptionWidth="w-80" />
        <div className="flex flex-wrap items-center gap-3">
          <div className="h-9 w-48 animate-pulse rounded-md bg-kumo-fill" />
          <div className="h-9 w-36 animate-pulse rounded-md bg-kumo-fill" />
          <div className="h-8 w-20 animate-pulse rounded-md bg-kumo-fill" />
        </div>
      </div>
      <StatCardsSkeleton count={3} columns="grid-cols-1 sm:grid-cols-3" />
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <CardBlockSkeleton lines={4} />
        <CardBlockSkeleton lines={4} />
      </div>
      <LayerCard>
        <LayerCard.Secondary className="block">
          <div className="flex flex-col gap-2">
            <SkeletonLine blockHeight={16} className="w-36" />
            <SkeletonLine blockHeight={12} className="w-56" />
          </div>
        </LayerCard.Secondary>
        <LayerCard.Primary>
          <div className="h-44 w-full animate-pulse rounded-md bg-kumo-fill" />
        </LayerCard.Primary>
      </LayerCard>
      <LayerCard>
        <LayerCard.Secondary className="block">
          <div className="flex flex-col gap-2">
            <SkeletonLine blockHeight={16} className="w-40" />
            <SkeletonLine blockHeight={12} className="w-64" />
          </div>
        </LayerCard.Secondary>
        <LayerCard.Primary>
          <TableRowsSkeleton rows={5} columns={6} />
        </LayerCard.Primary>
      </LayerCard>
    </div>
  );
}

export function LogsTableSkeleton({ rows = 10 }: { rows?: number }) {
  return (
    <div className="flex flex-col" aria-busy="true" aria-label="Loading requests">
      {Array.from({ length: rows }, (_, index) => (
        <div
          key={index}
          className="grid items-center gap-3 border-b border-kumo-hairline px-4 py-2.5"
          style={{ gridTemplateColumns: LOG_COLUMNS }}
        >
          <SkeletonLine className="h-3 w-12" />
          <SkeletonLine className="h-3 w-full max-w-[90%]" />
          <SkeletonLine className="h-3 w-20" />
          <SkeletonLine className="h-3 w-12" />
          <SkeletonLine className="h-3 w-10" />
          <SkeletonLine className="h-3 w-10" />
          <SkeletonLine className="h-3 w-10" />
          <SkeletonLine className="h-3 w-12" />
          <SkeletonLine className="h-3 w-10" />
          <SkeletonLine className="ml-auto h-3 w-8" />
        </div>
      ))}
    </div>
  );
}

export function LogDetailSkeleton() {
  return (
    <div className="flex min-w-0 flex-col gap-6" aria-busy="true" aria-label="Loading log detail">
      <div className="flex flex-wrap items-center gap-3">
        <SkeletonLine className="h-4 w-16" />
        <SkeletonLine className="h-6 w-48" />
        <div className="ml-auto h-8 w-28 animate-pulse rounded-md bg-kumo-fill" />
      </div>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {Array.from({ length: 4 }, (_, index) => (
          <div
            key={index}
            className="rounded-md border border-kumo-hairline bg-kumo-elevated px-3 py-2"
          >
            <SkeletonLine className="h-3 w-16" />
            <SkeletonLine className="mt-2 h-4 w-24" />
          </div>
        ))}
      </div>
      <CardBlockSkeleton lines={6} />
      <CardBlockSkeleton lines={8} />
    </div>
  );
}
