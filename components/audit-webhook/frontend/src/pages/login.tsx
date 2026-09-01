import { useState } from "react";
import { Button, Card, Form, Input, Typography, message } from "antd";
import { errMessage } from "../api";
import { mountPage } from "../app";

interface LoginForm {
  password: string;
}

function LoginPage() {
  const [submitting, setSubmitting] = useState(false);

  const onFinish = async (values: LoginForm) => {
    setSubmitting(true);
    try {
      const resp = await fetch("/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(values),
      });
      if (resp.status === 401) {
        message.error("密码错误");
        return;
      }
      if (!resp.ok) {
        throw new Error(`HTTP ${resp.status}`);
      }
      window.location.href = "/";
    } catch (err) {
      message.error(`登录失败：${errMessage(err)}`);
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="login-page">
      <Card className="login-card">
        <Typography.Title level={4} style={{ marginTop: 0 }}>
          OpenSandbox 审计
        </Typography.Title>
        <Typography.Paragraph type="secondary">
          输入密码登录沙箱访问审计系统
        </Typography.Paragraph>
        <Form<LoginForm> layout="vertical" onFinish={onFinish} autoComplete="off">
          <Form.Item
            name="password"
            label="密码"
            rules={[{ required: true, message: "请输入密码" }]}
          >
            <Input.Password placeholder="密码" autoFocus />
          </Form.Item>
          <Form.Item style={{ marginBottom: 0 }}>
            <Button type="primary" htmlType="submit" block loading={submitting}>
              登录
            </Button>
          </Form.Item>
        </Form>
      </Card>
    </div>
  );
}

mountPage(LoginPage);
