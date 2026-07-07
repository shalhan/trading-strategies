#!/usr/bin/env python3
"""Meta-labeling confidence model for the structure strategy (config 08b).

Protocol (fixed, to avoid the project's classic overfitting traps):
  TRAIN  on trades_2024_25.csv + trades_2025h2.csv (Jul 2024 - Jan 2026)
  TEST   on trades_2026h1.csv  (Jan - Jun 2026, untouched by training)
  FINAL  on trades_recent.csv  (last 30 days)

The model predicts P(win) from the entry-time feature snapshot. It earns its
place only if gating trades by confidence IMPROVES net expectancy on TEST
without destroying total R. Everything is reported per confidence bucket so
the decision is visible, not just an AUC.
"""
import sys
from pathlib import Path

import numpy as np
import pandas as pd
from sklearn.ensemble import GradientBoostingClassifier
from sklearn.linear_model import LogisticRegression
from sklearn.metrics import roc_auc_score
from sklearn.pipeline import make_pipeline
from sklearn.preprocessing import StandardScaler

HERE = Path(__file__).parent
LABELS = ["gross_r", "net_r", "win"]
META = ["symbol", "entry_time", "side", "setup"]


def load(name: str) -> pd.DataFrame:
    df = pd.read_csv(HERE / name)
    df["entry_time"] = pd.to_datetime(df["entry_time"])
    return df.sort_values("entry_time").reset_index(drop=True)


def xy(df: pd.DataFrame):
    feats = [c for c in df.columns if c not in LABELS + META]
    X = df[feats].fillna(0.0).values
    return X, df["win"].values, feats


def bucket_report(df: pd.DataFrame, proba: np.ndarray, name: str):
    d = df.copy()
    d["p"] = proba
    d["bucket"] = pd.qcut(d["p"], 4, labels=["Q1 low", "Q2", "Q3", "Q4 high"], duplicates="drop")
    print(f"\n  {name}: expectancy by confidence quartile")
    print("    bucket    n   win%   avg netR   sum netR   p-range")
    for b, g in d.groupby("bucket", observed=True):
        print(
            "    %-7s %4d  %4.0f%%   %+7.3f   %+8.2f   %.2f-%.2f"
            % (b, len(g), 100 * g["win"].mean(), g["net_r"].mean(), g["net_r"].sum(), g["p"].min(), g["p"].max())
        )


def threshold_report(df: pd.DataFrame, proba: np.ndarray):
    d = df.copy()
    d["p"] = proba
    base_total, base_exp = d["net_r"].sum(), d["net_r"].mean()
    print(f"\n  gate sweep (baseline: {len(d)} trades, {base_total:+.2f}R total, {base_exp:+.3f}R/trade)")
    print("    keep top   trades   total netR   netR/trade")
    for frac in (0.75, 0.5, 0.25):
        cut = d["p"].quantile(1 - frac)
        kept = d[d["p"] >= cut]
        print(
            "    %3.0f%%       %4d     %+8.2f     %+7.3f"
            % (100 * frac, len(kept), kept["net_r"].sum(), kept["net_r"].mean())
        )


def main():
    train = pd.concat([load("trades_2024_25.csv"), load("trades_2025h2.csv")], ignore_index=True)
    test = load("trades_2026h1.csv")
    final = load("trades_recent.csv")

    Xtr, ytr, feats = xy(train)
    print(f"train: {len(train)} trades ({train['win'].mean():.0%} win) | features: {len(feats)}")
    print(f"test : {len(test)} trades ({test['win'].mean():.0%} win)")
    print(f"final: {len(final)} trades ({final['win'].mean():.0%} win)")

    models = {
        "logistic": make_pipeline(StandardScaler(), LogisticRegression(C=0.3, max_iter=2000)),
        "gbm-tiny": GradientBoostingClassifier(
            n_estimators=150, max_depth=2, learning_rate=0.05, subsample=0.8, random_state=7
        ),
    }
    for name, model in models.items():
        model.fit(Xtr, ytr)
        print(f"\n================ {name} ================")
        for split_name, df in (("TEST 2026H1", test), ("FINAL recent", final)):
            X, y, _ = xy(df)
            p = model.predict_proba(X)[:, 1]
            try:
                auc = roc_auc_score(y, p)
            except ValueError:
                auc = float("nan")
            print(f"\n {split_name}: AUC {auc:.3f}")
            bucket_report(df, p, split_name)
            threshold_report(df, p)

        if name == "logistic":
            lr = model.named_steps["logisticregression"]
            coef = sorted(zip(feats, lr.coef_[0]), key=lambda t: -abs(t[1]))
            print("\n  logistic coefficients (standardized, |top 10|):")
            for f, c in coef[:10]:
                print(f"    {f:<16} {c:+.3f}")


if __name__ == "__main__":
    sys.exit(main())
