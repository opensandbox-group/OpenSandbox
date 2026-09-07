import { useCallback, useEffect, useRef, useState } from "react";
import {
  Button,
  DatePicker,
  Input,
  Popconfirm,
  Space,
  Switch,
  Table,
  Tag,
  Typography,
  message,
} from "antd";
import type { ColumnsType, TablePaginationConfig } from "antd/es/table";
import type { FilterValue, SorterResult } from "antd/es/table/interface";
import type { Dayjs } from "dayjs";
import { apiFetch, errMessage, fmtTime, logout } from "../api";
import { mountPage } from "../app";

interface SandboxRow {
  sandbox_id: string;
  uri: string | null;
  method: string | null;
  target: string | null;
  request_time: string | null;
  request_count: number;
  /** false = discovered in the cluster, no request recorded yet */
  accessed: boolean;
  /** BatchSandbox creationTimestamp, set on discovery-inserted rows */
  created_at: string | null;
  /** IP of the node the sandbox pod runs on (synced from the cluster) */
  node_ip: string | null;
  /** sandbox resource no longer exists in the cluster (已删除 marker) */
  deleted: boolean;
}

const { RangePicker } = DatePicker;
const PAGE_SIZE = 50;
/** Default server-side sort; cancelling a column sort falls back to it. */
const DEFAULT_SORT = "-request_time";
/**
 * Column-click cycle: 降序 → 升序 → 降序 ... The repeated third entry keeps
 * antd's nextSortDirection index in range, so the cycle never reaches its
 * "cancel sort" state. A cancel would force the sort somewhere else (the
 * server always needs a sort key), which reads as the indicator jumping
 * to the 最新请求时间 column - with this cycle the indicator stays on the
 * clicked column.
 */
const SORT_DIRECTIONS = ["descend", "ascend", "descend"] as const;

const METHOD_COLORS: Record<string, string> = {
  GET: "green",
  POST: "blue",
  PUT: "orange",
  DELETE: "red",
  PATCH: "purple",
  HEAD: "default",
  OPTIONS: "default",
};

function SummaryPage() {
  const [rows, setRows] = useState<SandboxRow[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const [searchInput, setSearchInput] = useState("");
  const [search, setSearch] = useState("");
  const [range, setRange] = useState<[Dayjs | null, Dayjs | null] | null>(null);
  const [sort, setSort] = useState<string>(DEFAULT_SORT);
  const [page, setPage] = useState(1);
  const [autoRefresh, setAutoRefresh] = useState(false);

  // Guards against out-of-order responses: only the latest load()'s result
  // is applied (rapid sort/search clicks fire overlapping requests).
  const loadSeq = useRef(0);

  const load = useCallback(async () => {
    const seq = ++loadSeq.current;
    setLoading(true);
    try {
      const params = new URLSearchParams({
        limit: String(PAGE_SIZE),
        offset: String((page - 1) * PAGE_SIZE),
        sort,
      });
      if (search) params.set("search", search);
      // Date pickers cover whole days in the browser's local timezone.
      if (range?.[0]) params.set("time_from", range[0].startOf("day").toISOString());
      if (range?.[1]) params.set("time_to", range[1].endOf("day").toISOString());
      const data = await apiFetch<{ total: number; items: SandboxRow[] }>(
        `/api/sandboxes?${params}`
      );
      if (seq !== loadSeq.current) return; // a newer request took over
      setRows(data.items);
      setTotal(data.total);
    } catch (err) {
      if (seq === loadSeq.current) {
        message.error(`加载沙箱总表失败：${errMessage(err)}`);
      }
    } finally {
      if (seq === loadSeq.current) setLoading(false);
    }
  }, [page, sort, search, range]);

  useEffect(() => {
    load();
  }, [load]);

  useEffect(() => {
    if (!autoRefresh) return;
    const timer = setInterval(load, 10000);
    return () => clearInterval(timer);
  }, [autoRefresh, load]);

  const applySearch = (value: string) => {
    setSearch(value.trim());
    setPage(1);
  };

  const resetFilters = () => {
    setSearchInput("");
    setSearch("");
    setRange(null);
    setSort(DEFAULT_SORT);
    setPage(1);
  };

  // The sortDirections cycle above never emits a "cancel sort" event, so
  // the sorter always carries a direction. An empty order (antd sends an
  // empty sorter object on a cancel) can only arrive as an edge case -
  // fall back to the default sort there rather than leaving the
  // controlled sortOrder stuck on the last direction.
  const onTableChange = (
    pagination: TablePaginationConfig,
    _filters: Record<string, FilterValue | null>,
    sorter: SorterResult<SandboxRow> | SorterResult<SandboxRow>[]
  ) => {
    const s = Array.isArray(sorter) ? sorter[0] : sorter;
    if (pagination.current) setPage(pagination.current);
    const nextSort = s?.order
      ? s.order === "ascend"
        ? String(s.field)
        : `-${s.field}`
      : DEFAULT_SORT;
    if (nextSort !== sort) {
      setSort(nextSort);
      setPage(1);
    }
  };

  const columns: ColumnsType<SandboxRow> = [
    {
      title: "沙箱 ID",
      dataIndex: "sandbox_id",
      key: "sandbox_id",
      render: (id: string) => (
        <Typography.Link
          onClick={() => {
            window.location.href = `/details?sandbox_id=${encodeURIComponent(id)}`;
          }}
        >
          {id}
        </Typography.Link>
      ),
    },
    {
      title: "状态",
      dataIndex: "accessed",
      key: "accessed",
      width: 90,
      sorter: true,
      sortDirections: [...SORT_DIRECTIONS],
      sortOrder:
        sort === "accessed" ? "ascend" : sort === "-accessed" ? "descend" : null,
      render: (accessed: boolean, row: SandboxRow) =>
        row.deleted ? (
          <Tag color="red">已删除</Tag>
        ) : accessed ? (
          <Tag color="green">已访问</Tag>
        ) : (
          <Tag color="orange">未访问</Tag>
        ),
    },
    {
      title: "创建时间",
      dataIndex: "created_at",
      key: "created_at",
      width: 180,
      sorter: true,
      sortDirections: [...SORT_DIRECTIONS],
      sortOrder:
        sort === "created_at" ? "ascend" : sort === "-created_at" ? "descend" : null,
      render: (iso: string | null) => fmtTime(iso),
    },
    {
      title: "节点 IP",
      dataIndex: "node_ip",
      key: "node_ip",
      width: 140,
      render: (ip: string | null) => ip ?? "-",
    },
    {
      title: "方法",
      dataIndex: "method",
      key: "method",
      width: 90,
      render: (method: string | null) =>
        method ? (
          <Tag color={METHOD_COLORS[method.toUpperCase()] ?? "default"}>
            {method}
          </Tag>
        ) : (
          "-"
        ),
    },
    {
      title: "目标",
      dataIndex: "target",
      key: "target",
      render: (target: string | null) => target ?? "-",
    },
    {
      title: "最新请求时间",
      dataIndex: "request_time",
      key: "request_time",
      width: 180,
      sorter: true,
      sortDirections: [...SORT_DIRECTIONS],
      sortOrder:
        sort === "request_time" ? "ascend" : sort === "-request_time" ? "descend" : null,
      render: (iso: string | null) => fmtTime(iso),
    },
    {
      title: "累计请求数",
      dataIndex: "request_count",
      key: "request_count",
      width: 120,
      sorter: true,
      sortDirections: [...SORT_DIRECTIONS],
      sortOrder:
        sort === "request_count" ? "ascend" : sort === "-request_count" ? "descend" : null,
      render: (count: number) => <Tag color="geekblue">{count}</Tag>,
    },
  ];

  return (
    <>
      <header className="app-header">
        <h1>OpenSandbox 沙箱最新请求（总表）</h1>
        <Space>
          <label>
            自动刷新（10s）{" "}
            <Switch size="small" checked={autoRefresh} onChange={setAutoRefresh} />
          </label>
          <Button onClick={load}>刷新</Button>
          <Popconfirm title="确定退出登录？" onConfirm={logout} okText="退出" cancelText="取消">
            <Button danger>退出登录</Button>
          </Popconfirm>
        </Space>
      </header>
      <main className="app-main">
        <div className="table-card">
          <div className="table-toolbar">
            <Input.Search
              placeholder="按沙箱 ID 模糊搜索，或输入节点 IP 精确查询"
              allowClear
              value={searchInput}
              onChange={(e) => setSearchInput(e.target.value)}
              onSearch={applySearch}
              style={{ width: 300 }}
            />
            <RangePicker
              placeholder={["最新请求开始日期", "结束日期"]}
              value={range}
              onChange={(value) => {
                setRange(value as [Dayjs | null, Dayjs | null] | null);
                setPage(1);
              }}
            />
            <Button onClick={resetFilters}>重置</Button>
            <div className="spacer" />
            <Typography.Text type="secondary">共 {total} 条</Typography.Text>
          </div>
          <p className="hint">
            点击沙箱 ID 查看该沙箱的请求详情；点击「状态」/「创建时间」/「最新请求时间」/「累计请求数」表头排序（降序 ↔ 升序循环）；
            「未访问」表示沙箱存在于集群但尚无访问记录；「已删除」表示沙箱资源已从集群移除（访问记录保留可查）；
            搜索框输入节点 IP 可查到该节点上的所有沙箱
          </p>
          <Table<SandboxRow>
            rowKey="sandbox_id"
            columns={columns}
            dataSource={rows}
            loading={loading}
            onChange={onTableChange}
            size="middle"
            pagination={{
              current: page,
              pageSize: PAGE_SIZE,
              total,
              showSizeChanger: false,
              showQuickJumper: true,
            }}
            locale={{ emptyText: "暂无记录" }}
          />
        </div>
      </main>
    </>
  );
}

mountPage(SummaryPage);
